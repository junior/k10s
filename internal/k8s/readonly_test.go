package k8s

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"

	"github.com/p10node/k10s/internal/domain"
)

func TestCheckReadOnlyAllowsOnlyReads(t *testing.T) {
	cases := []struct {
		method, path string
		allowed      bool
	}{
		{"GET", "/api/v1/namespaces/default/pods", true},
		{"GET", "/api/v1/namespaces/default/pods/web/log", true},
		{"GET", "/apis/metrics.k8s.io/v1beta1/nodes", true},
		{"HEAD", "/version", true},
		{"OPTIONS", "/api", true},
		// A pod named "exec" in a namespace named "pods" is still a read.
		{"GET", "/api/v1/namespaces/pods/pods/exec", true},
		{"POST", "/api/v1/namespaces/default/pods", false},
		{"PUT", "/apis/apps/v1/namespaces/default/deployments/web", false},
		{"PATCH", "/apis/apps/v1/namespaces/default/deployments/web/scale", false},
		{"DELETE", "/api/v1/namespaces/default/pods/web", false},
		{"POST", "/api/v1/namespaces/default/pods/web/eviction", false},
		{"POST", "/api/v1/namespaces/default/pods/web/exec", false},
		// WebSocket exec, attach and port-forward open with a GET.
		{"GET", "/api/v1/namespaces/default/pods/web/exec", false},
		{"GET", "/api/v1/namespaces/default/pods/web/attach", false},
		{"GET", "/api/v1/namespaces/default/pods/web/portforward", false},
	}
	for _, c := range cases {
		err := checkReadOnly(c.method, c.path)
		if c.allowed && err != nil {
			t.Errorf("%s %s refused: %v", c.method, c.path, err)
		}
		if !c.allowed && !errors.Is(err, domain.ErrReadOnly) {
			t.Errorf("%s %s = %v, want ErrReadOnly", c.method, c.path, err)
		}
	}
}

// recordingAPIServer answers every request with a 404 Status and remembers
// what reached it, so a test can prove a request never left the client.
func recordingAPIServer(t *testing.T) (*rest.Config, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`)
	}))
	t.Cleanup(srv.Close)
	cfg := &rest.Config{Host: srv.URL}
	guardReadOnly(cfg)
	return cfg, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestReadOnlyClientSendsReadsAndNoWrites(t *testing.T) {
	cfg, seen := recordingAPIServer(t)
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := cs.CoreV1().Pods("default").Delete(ctx, "web", metav1.DeleteOptions{}); !errors.Is(err, domain.ErrReadOnly) {
		t.Errorf("delete: err = %v, want ErrReadOnly", err)
	}
	if _, err := cs.AppsV1().Deployments("default").UpdateScale(ctx, "web", &autoscalingv1.Scale{}, metav1.UpdateOptions{}); !errors.Is(err, domain.ErrReadOnly) {
		t.Errorf("scale: err = %v, want ErrReadOnly", err)
	}
	if _, err := cs.CoreV1().Pods("default").Get(ctx, "web", metav1.GetOptions{}); errors.Is(err, domain.ErrReadOnly) {
		t.Errorf("get was refused: %v", err)
	}

	got := seen()
	if len(got) != 1 || got[0] != "GET /api/v1/namespaces/default/pods/web" {
		t.Errorf("requests that reached the API server = %v, want only the GET", got)
	}
}

// exec and port-forward build their own transports from the config, over
// WebSockets or SPDY; the guard has to sit under those too.
func TestReadOnlyClientOpensNoExecOrPortForward(t *testing.T) {
	cfg, seen := recordingAPIServer(t)

	execURL, err := url.Parse(cfg.Host + "/api/v1/namespaces/default/pods/web/exec?command=sh&stdin=true&stdout=true&tty=true")
	if err != nil {
		t.Fatal(err)
	}
	executor, err := newExecutor(cfg, execURL)
	if err != nil {
		t.Fatal(err)
	}
	err = executor.StreamWithContext(context.Background(), remotecommand.StreamOptions{
		Stdin: strings.NewReader(""), Stdout: io.Discard, Tty: true,
	})
	if err == nil || !strings.Contains(err.Error(), domain.ErrReadOnly.Error()) {
		t.Errorf("exec: err = %v, want a read-only refusal", err)
	}

	transport, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pfURL, err := url.Parse(cfg.Host + "/api/v1/namespaces/default/pods/web/portforward")
	if err != nil {
		t.Fatal(err)
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, pfURL)
	if _, _, err := dialer.Dial("portforward.k8s.io"); err == nil || !strings.Contains(err.Error(), domain.ErrReadOnly.Error()) {
		t.Errorf("port-forward: err = %v, want a read-only refusal", err)
	}

	if got := seen(); len(got) != 0 {
		t.Errorf("requests that reached the API server = %v, want none", got)
	}
}
