package agent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRemoveWithVolumes(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[
		  {"Id":"c1","Names":["/old"],"Mounts":[
		    {"Type":"volume","Name":"own"},{"Type":"volume","Name":"shared"},{"Type":"bind","Name":"","Source":"/etc"}]},
		  {"Id":"c2","Names":["/other"],"Mounts":[{"Type":"volume","Name":"shared"}]}]`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: mux}}
	srv.Start()
	defer srv.Close()
	d, err := NewDocker("unix://" + sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := d.RemoveWithVolumes(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"DELETE /containers/c1", "DELETE /volumes/own"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls: %v", calls)
	}
	if !strings.Contains(out, "deleted volume own") || !strings.Contains(out, "kept volume shared") {
		t.Fatalf("output: %q", out)
	}
}
