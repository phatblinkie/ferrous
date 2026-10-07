package docker

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func detailFromJSON(t *testing.T, s string) *ContainerDetail {
	t.Helper()
	var d ContainerDetail
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		t.Fatal(err)
	}
	return &d
}

func TestRconConfigDefaults(t *testing.T) {
	d := detailFromJSON(t, `{
		"Config": {"Labels": {"ferrous.rcon_port": ""}, "Env": ["RCON_PASSWORD=s3cret"]},
		"NetworkSettings": {"IPAddress": "172.18.0.7"}
	}`)
	rc, err := rconConfigFrom(d)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Addr != "172.18.0.7:28016" || rc.Password != "s3cret" {
		t.Fatalf("got %+v", rc)
	}
}

func TestRconConfigLabelPort(t *testing.T) {
	d := detailFromJSON(t, `{
		"Config": {"Labels": {"ferrous.rcon_port": "3333"}, "Env": ["RCON_PASSWORD=p"]},
		"NetworkSettings": {"IPAddress": "10.0.0.9"}
	}`)
	rc, err := rconConfigFrom(d)
	if err != nil || rc.Addr != "10.0.0.9:3333" {
		t.Fatalf("got %+v, %v", rc, err)
	}
}

func TestRconConfigNetworksFallback(t *testing.T) {
	// modern docker: root IPAddress empty, per-network addresses under Networks
	d := detailFromJSON(t, `{
		"Config": {"Env": ["RCON_PASSWORD=p"]},
		"NetworkSettings": {"IPAddress": "", "Networks": {"ferrous_net": {"IPAddress": "172.19.0.4"}}}
	}`)
	rc, err := rconConfigFrom(d)
	if err != nil || rc.Addr != "172.19.0.4:28016" {
		t.Fatalf("got %+v, %v", rc, err)
	}
}

func TestRconConfigMissingPassword(t *testing.T) {
	d := detailFromJSON(t, `{"Config": {"Env": ["PATH=/bin"]}, "NetworkSettings": {"IPAddress": "1.2.3.4"}}`)
	_, err := rconConfigFrom(d)
	if !errors.Is(err, ErrNoRCON) {
		t.Fatalf("want ErrNoRCON, got %v", err)
	}
}

func TestRconConfigBadLabel(t *testing.T) {
	d := detailFromJSON(t, `{
		"Config": {"Labels": {"ferrous.rcon_port": "http"}, "Env": ["RCON_PASSWORD=p"]},
		"NetworkSettings": {"IPAddress": "1.2.3.4"}
	}`)
	_, err := rconConfigFrom(d)
	if err == nil || !strings.Contains(err.Error(), "bad ferrous.rcon_port") {
		t.Fatalf("want bad-label error, got %v", err)
	}
}

func TestRconConfigNoIP(t *testing.T) {
	d := detailFromJSON(t, `{"Config": {"Env": ["RCON_PASSWORD=p"]}, "NetworkSettings": {}}`)
	_, err := rconConfigFrom(d)
	if err == nil || !strings.Contains(err.Error(), "no IP") {
		t.Fatalf("want no-IP error, got %v", err)
	}
}

func TestEnvValue(t *testing.T) {
	env := []string{"PATH=/bin", "RCON_PASSWORD=hunter2", "RCON_PASSWORD_EXTRA=x"}
	v, ok := EnvValue(env, "RCON_PASSWORD")
	if !ok || v != "hunter2" {
		t.Fatalf("got %q %v", v, ok)
	}
	if _, ok := EnvValue(env, "NOPE"); ok {
		t.Fatal("missing key must report false")
	}
}
