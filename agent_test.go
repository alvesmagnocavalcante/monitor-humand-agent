package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAlertAndRearm(t *testing.T) {
	active := map[string]bool{}
	readings := []metric{{name: "CPU", usage: 90, limit: 90}, {name: "RAM", usage: 89, limit: 90}}
	alerts := newAlerts(readings, active)
	if len(alerts) != 1 || alerts[0] != "CPU" {
		t.Fatalf("alertas inesperados: %v", alerts)
	}
	active["CPU"] = true
	if len(newAlerts(readings, active)) != 0 {
		t.Fatal("alerta repetido")
	}
	newAlerts([]metric{{name: "CPU", err: errors.New("coleta falhou")}}, active)
	if !active["CPU"] {
		t.Fatal("erro de coleta rearmou alerta")
	}
	newAlerts([]metric{{name: "CPU", usage: 89, limit: 90}}, active)
	if len(newAlerts(readings, active)) != 1 {
		t.Fatal("novo episodio nao detectado")
	}
}

func TestPublish(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/groups/578650/posts" || r.Header.Get("Authorization") != "Basic fake-key" {
			t.Error("requisicao incorreta")
		}
		var body struct {
			Body   string `json:"body"`
			Notify bool   `json:"sendNotification"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Body != "alerta" || !body.Notify {
			t.Error("payload incorreto")
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":123}`))
	}))
	defer server.Close()
	cfg := config{endpoint: server.URL + "/groups/578650/posts", authorization: "Basic fake-key"}
	id, err := publish(context.Background(), server.Client(), cfg, "alerta")
	if err != nil || id != 123 {
		t.Fatalf("id=%d erro=%v", id, err)
	}
}

func TestPublishErrors(t *testing.T) {
	for _, status := range []int{201, 403, 429, 500} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{}`))
		}))
		_, err := publish(context.Background(), server.Client(), config{endpoint: server.URL}, "alerta")
		server.Close()
		if err == nil {
			t.Fatalf("HTTP %d nao validado", status)
		}
		if status == 429 {
			var failure *apiError
			if !errors.As(err, &failure) || failure.delay != 120*time.Second {
				t.Fatalf("Retry-After incorreto: %v", err)
			}
		}
	}
}
