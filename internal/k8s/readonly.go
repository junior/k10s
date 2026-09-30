package k8s

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"

	"k8s.io/client-go/rest"

	"github.com/p10node/k10s/internal/domain"
)

// readOnly is set once by main, from --readonly, before any client exists.
var readOnly atomic.Bool

// SetReadOnly makes every client built after it refuse requests that could
// change the cluster or open a session into it. Clients that already exist
// keep their transport, so main calls it before the first connection.
func SetReadOnly(on bool) { readOnly.Store(on) }

// guardReadOnly puts readOnlyTransport under every client built from cfg:
// the clientset, the dynamic client, discovery, metrics, and the exec and
// port-forward streams, which client-go builds from the same config.
func guardReadOnly(cfg *rest.Config) {
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return readOnlyTransport{next: rt}
	})
}

// readOnlyTransport refuses a request before it leaves the machine unless it
// can only read. It is the backstop behind the UI, which hides and refuses
// the same actions: whichever path a request takes, the API server never
// receives a write.
type readOnlyTransport struct{ next http.RoundTripper }

func (t readOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := checkReadOnly(req.Method, req.URL.Path); err != nil {
		return nil, err
	}
	return t.next.RoundTrip(req)
}

// checkReadOnly allows GET, HEAD and OPTIONS, except where a GET opens a
// session: exec, attach and port-forward upgrade to a stream over GET when
// they use WebSockets. Every other method can change something.
func checkReadOnly(method, path string) error {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		if sub := streamSubresource(path); sub != "" {
			return fmt.Errorf("%w: pod %s is not allowed", domain.ErrReadOnly, sub)
		}
		return nil
	}
	return fmt.Errorf("%w: %s %s is not allowed", domain.ErrReadOnly, method, path)
}

// streamSubresource returns "exec", "attach" or "portforward" when path is
// that subresource of a pod, /api/v1/namespaces/<ns>/pods/<name>/<sub>, and
// "" otherwise. Anchoring on "namespaces" keeps a pod that happens to be
// named "exec" readable.
func streamSubresource(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	n := len(segs)
	if n < 5 || segs[n-5] != "namespaces" || segs[n-3] != "pods" {
		return ""
	}
	switch segs[n-1] {
	case "exec", "attach", "portforward":
		return segs[n-1]
	}
	return ""
}
