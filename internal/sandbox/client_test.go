package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/BingyanStudio/CloudPilot/internal/sandbox/process"
	"github.com/BingyanStudio/CloudPilot/internal/sandbox/process/processconnect"
)

func TestProviderLifecycleProfiles(t *testing.T) {
	for _, provider := range []string{"e2b", "novita"} {
		t.Run(provider, func(t *testing.T) {
			prefix := "/v2/sandboxes"
			if provider == "novita" {
				prefix = "/sandboxes"
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.Header.Get("X-API-KEY") != "api-secret" {
					t.Error("invalid control request")
				}
				var body map[string]any
				if r.Body != nil && r.ContentLength != 0 {
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
				}
				switch r.URL.Path {
				case prefix:
					if body["templateID"] != "worker-template" || body["timeout"] != float64(300) || body["autoPause"] != true {
						t.Error("invalid create options")
					}
					if _, present := body["envVars"]; present {
						t.Error("nil environment must not be serialized")
					}
					if provider == "novita" {
						if body["secure"] != true {
							t.Error("Novita requires secure envd")
						}
					} else {
						if _, present := body["secure"]; present {
							t.Error("E2B v2 has no secure field")
						}
						if body["autoPauseMemory"] != true {
							t.Error("E2B must retain memory on lease expiry")
						}
					}
				case prefix + "/test-id/connect":
					if body["timeout"] != float64(300) {
						t.Error("invalid reconnect lease")
					}
				case "/sandboxes/test-id/pause":
					if provider == "e2b" && body["memory"] != true {
						t.Error("E2B pause must retain memory")
					}
					if provider == "novita" && body != nil {
						t.Error("Novita pause should omit the E2B body")
					}
				default:
					t.Errorf("unexpected provider route: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				calls++
				fmt.Fprint(w, `{"sandboxID":"test-id","envdVersion":"0.5.12","envdAccessToken":"scope-secret"}`)
			}))
			defer server.Close()
			client := &Client{Provider: provider, APIURL: server.URL, APIKey: "api-secret"}
			sb, err := client.Create(t.Context(), CreateOptions{TemplateID: "worker-template", Timeout: 5 * time.Minute, PauseOnTimeout: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = client.Connect(t.Context(), sb.ID, 5*time.Minute); err != nil {
				t.Fatal(err)
			}
			if err = client.Pause(t.Context(), sb.ID); err != nil {
				t.Fatal(err)
			}
			if calls != 3 {
				t.Fatalf("expected three lifecycle operations, got %d", calls)
			}
		})
	}
}

func TestControlPaginationAndCredentialSeparation(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-KEY") != "control-secret" {
			t.Error("missing control authentication")
		}
		if r.URL.Path != "/v2/sandboxes" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "control-secret")
			return
		}
		if r.URL.Query().Get("state") != "running,paused" || r.URL.Query().Get("metadata") != "deployment=example" {
			t.Error("invalid list filter")
		}
		calls++
		if calls == 1 {
			w.Header().Set("X-Next-Token", "page-2")
			fmt.Fprint(w, `[{"sandboxID":"running-1","state":"running"}]`)
			return
		}
		if r.URL.Query().Get("nextToken") != "page-2" {
			t.Error("missing next page token")
		}
		fmt.Fprint(w, `[{"sandboxID":"paused-1","state":"paused"}]`)
	}))
	defer server.Close()
	c := &Client{APIURL: server.URL, APIKey: "control-secret"}
	items, err := c.List(t.Context(), map[string]string{"deployment": "example"})
	if err != nil || len(items) != 2 || items[1].State != "paused" {
		t.Fatalf("list: %v, %v", items, err)
	}
	_, err = c.Get(t.Context(), "missing")
	if !IsNotFound(err) || strings.Contains(err.Error(), "control-secret") {
		t.Fatalf("unsafe or untyped error: %v", err)
	}
	if err = c.Kill(t.Context(), "missing"); err != nil {
		t.Fatal(err)
	}
}

type processFixture struct {
	processconnect.UnimplementedProcessHandler
	input    bytes.Buffer
	eof      chan struct{}
	truncate bool
}

func (f *processFixture) Start(ctx context.Context, r *connect.Request[pb.StartRequest], stream *connect.ServerStream[pb.StartResponse]) error {
	if r.Header().Get("X-Access-Token") != "sandbox-secret" || r.Header().Get("X-API-KEY") != "" {
		return connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("invalid scope"))
	}
	if r.Msg.Pty != nil || !r.Msg.GetStdin() {
		return fmt.Errorf("expected non-PTY stdin")
	}
	if err := stream.Send(&pb.StartResponse{Event: &pb.ProcessEvent{Event: &pb.ProcessEvent_Start{Start: &pb.ProcessEvent_StartEvent{Pid: 42}}}}); err != nil {
		return err
	}
	if f.truncate {
		return nil
	}
	select {
	case <-f.eof:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := stream.Send(&pb.StartResponse{Event: &pb.ProcessEvent{Event: &pb.ProcessEvent_Data{Data: &pb.ProcessEvent_DataEvent{Output: &pb.ProcessEvent_DataEvent_Stdout{Stdout: f.input.Bytes()}}}}}); err != nil {
		return err
	}
	return stream.Send(&pb.StartResponse{Event: &pb.ProcessEvent{Event: &pb.ProcessEvent_End{End: &pb.ProcessEvent_EndEvent{Exited: true, ExitCode: 7}}}})
}

func (f *processFixture) SendInput(_ context.Context, r *connect.Request[pb.SendInputRequest]) (*connect.Response[pb.SendInputResponse], error) {
	f.input.Write(r.Msg.Input.GetStdin())
	return connect.NewResponse(&pb.SendInputResponse{}), nil
}

func (f *processFixture) CloseStdin(context.Context, *connect.Request[pb.CloseStdinRequest]) (*connect.Response[pb.CloseStdinResponse], error) {
	close(f.eof)
	return connect.NewResponse(&pb.CloseStdinResponse{}), nil
}

func TestProcessInputEOFAndDroppedTransport(t *testing.T) {
	for _, truncate := range []bool{false, true} {
		t.Run(fmt.Sprint(truncate), func(t *testing.T) {
			fixture := &processFixture{eof: make(chan struct{}), truncate: truncate}
			_, handler := processconnect.NewProcessHandler(fixture)
			server := httptest.NewServer(handler)
			defer server.Close()
			s := &Sandbox{ID: "test", EnvdVersion: "0.5.12", client: &Client{}, accessToken: "sandbox-secret", envdURL: server.URL}
			var output bytes.Buffer
			p, err := s.Start(t.Context(), Command{Path: "cat", Stdin: true, Stdout: &output})
			if err != nil {
				t.Fatal(err)
			}
			if !truncate {
				if _, err = io.WriteString(p, "one\n"); err != nil {
					t.Fatal(err)
				}
				if _, err = io.WriteString(p, "two\n"); err != nil {
					t.Fatal(err)
				}
				if err = p.Close(); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			code, err := p.Wait(ctx)
			if truncate {
				if err == nil {
					t.Fatal("dropped transport treated as successful exit")
				}
				return
			}
			if err != nil || code != 7 || output.String() != "one\ntwo\n" {
				t.Fatalf("code %d, error %v, output %q", code, err, output.String())
			}
		})
	}
}
