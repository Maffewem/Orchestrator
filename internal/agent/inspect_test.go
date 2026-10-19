package agent

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestSpecFromInspect(t *testing.T) {
	var c inspectDoc
	err := json.Unmarshal([]byte(`{
	  "Name": "/web", "Image": "sha256:abc",
	  "Config": {"Image": "nginx:1.27", "Env": ["PATH=/usr/bin", "TOKEN=x"], "Cmd": ["nginx", "-g", "daemon off;"],
	    "Labels": {"maintainer": "nginx", "traefik.enable": "true", "com.docker.compose.project": "site"}},
	  "HostConfig": {"RestartPolicy": {"Name": "always"}, "NetworkMode": "site_default", "Privileged": true,
	    "PortBindings": {"80/tcp": [{"HostIp": "", "HostPort": "8080"}], "53/udp": [{"HostIp": "127.0.0.1", "HostPort": "53"}]}},
	  "Mounts": [{"Type": "volume", "Name": "data", "Destination": "/data", "RW": true},
	    {"Type": "bind", "Source": "/etc/conf", "Destination": "/conf", "RW": false},
	    {"Type": "tmpfs", "Destination": "/tmp", "RW": true}],
	  "NetworkSettings": {"Networks": {"site_default": {}}}
	}`), &c)
	if err != nil {
		t.Fatal(err)
	}
	var img imageDoc
	img.Config.Env = []string{"PATH=/usr/bin"}
	img.Config.Cmd = []string{"nginx", "-g", "daemon off;"}
	img.Config.Labels = map[string]string{"maintainer": "nginx"}

	got := specFromInspect(c, img)
	if got.Name != "web" || got.Image != "nginx:1.27" || got.Restart != "always" || got.Network != "site_default" {
		t.Fatalf("basics: %+v", got)
	}
	if !reflect.DeepEqual(got.Env, []string{"TOKEN=x"}) || got.Command != nil {
		t.Fatalf("env/cmd: %+v %v", got.Env, got.Command)
	}
	if !reflect.DeepEqual(got.Labels, map[string]string{"traefik.enable": "true"}) {
		t.Fatalf("labels: %v", got.Labels)
	}
	if !reflect.DeepEqual(got.Ports, []string{"53:53/udp", "8080:80"}) {
		t.Fatalf("ports: %v", got.Ports)
	}
	if !reflect.DeepEqual(got.Volumes, []string{"data:/data", "/etc/conf:/conf:ro"}) {
		t.Fatalf("volumes: %v", got.Volumes)
	}
	if len(got.Unsupported) != 3 { // 127.0.0.1-only port, tmpfs mount, privileged
		t.Fatalf("unsupported: %v", got.Unsupported)
	}
}
