package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	"golang.org/x/sys/windows"
)

type config struct {
	endpoint, authorization       string
	interval                      time.Duration
	cpuLimit, ramLimit, diskLimit float64
}

type metric struct {
	name         string
	usage, limit float64
	err          error
}

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	envPath := flag.String("env", defaultEnvPath(), "Arquivo .env")
	once := flag.Bool("once", false, "Executa uma coleta e encerra")
	dryRun := flag.Bool("dry-run", false, "Mostra as metricas sem publicar")
	flag.Parse()
	if err := loadEnv(*envPath); err != nil {
		return err
	}
	cfg, err := loadConfig(!*dryRun)
	if err != nil {
		return err
	}
	host, err := os.Hostname()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	active := make(map[string]bool)
	log.Printf("Monitorando %s; intervalo %s; limites CPU %.1f%%, RAM %.1f%%, disco %.1f%%", host, cfg.interval, cfg.cpuLimit, cfg.ramLimit, cfg.diskLimit)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		readings := collect(ctx, cfg)
		alerts := newAlerts(readings, active)
		body := report(host, readings, alerts)
		delay := cfg.interval
		var sendErr error
		if *dryRun {
			fmt.Println(body)
		} else if len(alerts) > 0 {
			var id uint64
			id, sendErr = publish(ctx, client, cfg, body)
			if sendErr == nil {
				log.Printf("Alerta publicado: %d", id)
			}
		}
		var failure *apiError
		if errors.As(sendErr, &failure) && failure.status == 429 {
			if failure.delay > delay {
				delay = failure.delay
			}
			log.Printf("Limite da API; nova coleta em %s", delay)
		} else {
			// Suppress repeats even if the POST result is uncertain.
			for _, name := range alerts {
				active[name] = true
			}
			if sendErr != nil {
				log.Printf("Falha de envio: %v. Verifique o grupo; este episodio nao sera repetido.", sendErr)
				if failure != nil && (failure.status == 401 || failure.status == 403 || failure.status == 404) {
					return sendErr
				}
			}
		}
		if *once {
			return sendErr
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func collect(ctx context.Context, cfg config) []metric {
	readings := []metric{{name: "CPU", limit: cfg.cpuLimit}, {name: "RAM", limit: cfg.ramLimit}}
	percent, err := cpu.PercentWithContext(ctx, time.Second, false)
	readings[0].err = err
	if err == nil && len(percent) == 1 {
		readings[0].usage = percent[0]
	} else if err == nil {
		readings[0].err = errors.New("CPU sem leitura")
	}
	ram, err := mem.VirtualMemoryWithContext(ctx)
	readings[1].err = err
	if err == nil {
		readings[1].usage = ram.UsedPercent
	}
	partitions, err := disk.PartitionsWithContext(ctx, false)
	if err != nil {
		log.Printf("Falha ao listar discos: %v", err)
	}
	for _, partition := range partitions {
		if partition.Mountpoint == "" {
			continue
		}
		path := strings.TrimRight(partition.Mountpoint, "\\/") + "\\"
		ptr, err := windows.UTF16PtrFromString(path)
		if err != nil || windows.GetDriveType(ptr) != windows.DRIVE_FIXED {
			continue
		}
		reading := metric{name: "Disco " + path, limit: cfg.diskLimit}
		usage, err := disk.UsageWithContext(ctx, path)
		reading.err = err
		if err == nil {
			reading.usage = usage.UsedPercent
		}
		readings = append(readings, reading)
	}
	return readings
}

func newAlerts(readings []metric, active map[string]bool) []string {
	var alerts []string
	for _, reading := range readings {
		if reading.err != nil || math.IsNaN(reading.usage) || math.IsInf(reading.usage, 0) {
			continue
		}
		if reading.usage < reading.limit {
			delete(active, reading.name)
		} else if !active[reading.name] {
			alerts = append(alerts, reading.name)
		}
	}
	return alerts
}

func report(host string, readings []metric, alerts []string) string {
	var body strings.Builder
	title := "Monitoramento do sistema"
	if len(alerts) > 0 {
		title = "Alerta de sistema"
	}
	fmt.Fprintf(&body, "%s\nMáquina: %s\n\n", title, host)
	var attention, summary strings.Builder
	for _, reading := range readings {
		name := strings.TrimRight(reading.name, "\\:")
		if reading.err != nil {
			log.Printf("Falha de coleta de %s: %v", reading.name, reading.err)
			fmt.Fprintf(&summary, "• %s: indisponível\n", name)
		} else if reading.usage >= reading.limit {
			fmt.Fprintf(&attention, "• %s: %s de uso (limite: %s)\n", name, percent(reading.usage), percent(reading.limit))
		} else {
			fmt.Fprintf(&summary, "• %s: %s de uso\n", name, percent(reading.usage))
		}
	}
	if attention.Len() > 0 {
		fmt.Fprintf(&body, "Limites atingidos\n%s\n", attention.String())
	}
	if summary.Len() > 0 {
		fmt.Fprintf(&body, "Demais recursos\n%s\n", summary.String())
	}
	fmt.Fprintf(&body, "Coleta: %s", time.Now().Format("02/01/2006 às 15:04:05"))
	return body.String()
}

func percent(value float64) string {
	return strings.ReplaceAll(strings.TrimSuffix(strconv.FormatFloat(value, 'f', 1, 64), ".0"), ".", ",") + "%"
}

type apiError struct {
	status int
	delay  time.Duration
}

func (e *apiError) Error() string { return fmt.Sprintf("API Humand HTTP %d", e.status) }

func publish(ctx context.Context, client *http.Client, cfg config, body string) (uint64, error) {
	data, err := json.Marshal(map[string]any{"body": body, "sendNotification": true})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.endpoint, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", cfg.authorization)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return 0, errors.New("falha de conexao; envio nao confirmado")
	}
	defer response.Body.Close()
	if response.StatusCode == 429 {
		return 0, &apiError{status: 429, delay: retryAfter(response.Header.Get("Retry-After"))}
	}
	if response.StatusCode != 201 {
		return 0, &apiError{status: response.StatusCode}
	}
	var post struct {
		ID uint64 `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&post); err != nil || post.ID == 0 {
		return 0, errors.New("HTTP 201 sem ID valido; verifique a publicacao no grupo")
	}
	return post.ID, nil
}

func retryAfter(value string) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 && seconds <= 86400 {
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil && deadline.After(time.Now()) {
		return time.Until(deadline)
	}
	return time.Minute
}

func loadConfig(requireKey bool) (config, error) {
	cfg := config{}
	key, scheme := strings.TrimSpace(os.Getenv("HUMAND_API_KEY")), setting("HUMAND_AUTH_SCHEME", "Basic")
	if requireKey && key == "" {
		return cfg, errors.New("preencha HUMAND_API_KEY no .env")
	}
	if strings.ContainsAny(key, "\r\n") || (scheme != "Basic" && scheme != "Bearer") {
		return cfg, errors.New("autenticacao invalida")
	}
	group, err := strconv.ParseUint(os.Getenv("HUMAND_GROUP_ID"), 10, 64)
	if err != nil || group == 0 {
		return cfg, errors.New("HUMAND_GROUP_ID deve ser inteiro positivo")
	}
	cfg.endpoint = fmt.Sprintf("https://api-prod.humand.co/public/api/v1/groups/%d/posts", group)
	cfg.authorization = scheme + " " + key
	cfg.interval, err = time.ParseDuration(setting("AGENT_INTERVAL", "30s"))
	if err != nil || cfg.interval < time.Second {
		return cfg, errors.New("AGENT_INTERVAL deve ser >= 1s, ex.: 30s")
	}
	for name, limit := range map[string]*float64{"AGENT_CPU_HIGH": &cfg.cpuLimit, "AGENT_RAM_HIGH": &cfg.ramLimit, "AGENT_DISK_HIGH": &cfg.diskLimit} {
		*limit, err = strconv.ParseFloat(setting(name, "90"), 64)
		if err != nil || math.IsNaN(*limit) || math.IsInf(*limit, 0) || *limit <= 0 || *limit > 100 {
			return cfg, fmt.Errorf("%s deve estar entre 0 (exclusivo) e 100", name)
		}
	}
	return cfg, nil
}

func setting(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func defaultEnvPath() string {
	if executable, err := os.Executable(); err == nil {
		path := filepath.Join(filepath.Dir(executable), ".env")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ".env"
}

func loadEnv(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	seen := make(map[string]bool)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\uFEFF"))
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		name, value, found := strings.Cut(text, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !found || name == "" || seen[name] {
			return fmt.Errorf(".env invalido na linha %d", line)
		}
		seen[name] = true
		if strings.HasPrefix(value, "\"") || strings.HasPrefix(value, "'") {
			if len(value) < 2 || value[len(value)-1] != value[0] {
				return fmt.Errorf("aspas invalidas no .env, linha %d", line)
			}
			value = value[1 : len(value)-1]
		}
		if _, exists := os.LookupEnv(name); !exists {
			if err := os.Setenv(name, value); err != nil {
				return fmt.Errorf("variavel invalida no .env, linha %d", line)
			}
		}
	}
	return scanner.Err()
}
