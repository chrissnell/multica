package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPostJSON(t *testing.T) {
	type reqBody struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	type respBody struct {
		ID string `json:"id"`
	}

	t.Run("success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			if ct := r.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("expected Content-Type application/json, got %s", ct)
			}
			if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
				t.Errorf("expected Authorization Bearer test-token, got %s", auth)
			}

			var body reqBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("failed to decode request body: %v", err)
			}
			if body.Name != "alice" || body.Age != 30 {
				t.Errorf("unexpected body: %+v", body)
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(respBody{ID: "123"})
		}))
		defer srv.Close()

		client := NewAPIClient(srv.URL, "", "test-token")
		var out respBody
		err := client.PostJSON(context.Background(), "/test", reqBody{Name: "alice", Age: 30}, &out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.ID != "123" {
			t.Errorf("expected ID 123, got %s", out.ID)
		}
	})

	t.Run("error status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, "bad request")
		}))
		defer srv.Close()

		client := NewAPIClient(srv.URL, "", "test-token")
		err := client.PostJSON(context.Background(), "/test", reqBody{Name: "bob"}, nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if got := err.Error(); got != "POST /test returned 400: bad request" {
			t.Errorf("unexpected error message: %s", got)
		}
	})

	t.Run("nil output", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
		}))
		defer srv.Close()

		client := NewAPIClient(srv.URL, "", "test-token")
		err := client.PostJSON(context.Background(), "/test", reqBody{Name: "charlie"}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("workspace and agent context headers", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ws := r.Header.Get("X-Workspace-ID"); ws != "ws-abc" {
				t.Errorf("expected X-Workspace-ID ws-abc, got %s", ws)
			}
			if agent := r.Header.Get("X-Agent-ID"); agent != "agent-123" {
				t.Errorf("expected X-Agent-ID agent-123, got %s", agent)
			}
			if task := r.Header.Get("X-Task-ID"); task != "task-456" {
				t.Errorf("expected X-Task-ID task-456, got %s", task)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(respBody{ID: "456"})
		}))
		defer srv.Close()

		client := NewAPIClient(srv.URL, "ws-abc", "test-token")
		client.AgentID = "agent-123"
		client.TaskID = "task-456"
		var out respBody
		err := client.PostJSON(context.Background(), "/test", reqBody{}, &out)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("client identity headers", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("X-Client-Platform"); got != "cli-test" {
				t.Errorf("expected X-Client-Platform cli-test, got %s", got)
			}
			if got := r.Header.Get("X-Client-Version"); got != "9.9.9" {
				t.Errorf("expected X-Client-Version 9.9.9, got %s", got)
			}
			if got := r.Header.Get("X-Client-OS"); got != "linux" {
				t.Errorf("expected X-Client-OS linux, got %s", got)
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()

		client := NewAPIClient(srv.URL, "", "")
		client.Platform = "cli-test"
		client.Version = "9.9.9"
		client.OS = "linux"
		if err := client.PostJSON(context.Background(), "/test", reqBody{}, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("client identity headers fall back to package defaults", func(t *testing.T) {
		origPlatform, origVersion, origOS := ClientPlatform, ClientVersion, ClientOS
		ClientPlatform = "cli"
		ClientVersion = "1.2.3-test"
		ClientOS = "macos"
		t.Cleanup(func() {
			ClientPlatform, ClientVersion, ClientOS = origPlatform, origVersion, origOS
		})

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("X-Client-Platform"); got != "cli" {
				t.Errorf("expected X-Client-Platform cli, got %s", got)
			}
			if got := r.Header.Get("X-Client-Version"); got != "1.2.3-test" {
				t.Errorf("expected X-Client-Version 1.2.3-test, got %s", got)
			}
			if got := r.Header.Get("X-Client-OS"); got != "macos" {
				t.Errorf("expected X-Client-OS macos, got %s", got)
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()

		client := NewAPIClient(srv.URL, "", "")
		if err := client.PostJSON(context.Background(), "/test", reqBody{}, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestDeleteJSONResponse(t *testing.T) {
	type respBody struct {
		ID string `json:"id"`
	}

	t.Run("success decodes response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				t.Errorf("expected DELETE, got %s", r.Method)
			}
			if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
				t.Errorf("expected Authorization Bearer test-token, got %s", auth)
			}
			json.NewEncoder(w).Encode(respBody{ID: "comment-123"})
		}))
		defer srv.Close()

		client := NewAPIClient(srv.URL, "", "test-token")
		var out respBody
		if err := client.DeleteJSONResponse(context.Background(), "/test", &out); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.ID != "comment-123" {
			t.Errorf("expected ID comment-123, got %s", out.ID)
		}
	})

	t.Run("error status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "missing")
		}))
		defer srv.Close()

		client := NewAPIClient(srv.URL, "", "test-token")
		err := client.DeleteJSONResponse(context.Background(), "/test", nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if got := err.Error(); got != "DELETE /test returned 404: missing" {
			t.Errorf("unexpected error message: %s", got)
		}
	})
}

func TestDownloadFile(t *testing.T) {
	t.Run("relative URL is resolved against BaseURL and sent with auth", func(t *testing.T) {
		var gotPath, gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			w.Write([]byte("hello"))
		}))
		defer srv.Close()

		client := NewAPIClient(srv.URL, "", "test-token")
		data, err := client.DownloadFile(context.Background(), "/uploads/workspaces/abc/file.md")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(data) != "hello" {
			t.Errorf("unexpected body: %q", string(data))
		}
		if gotPath != "/uploads/workspaces/abc/file.md" {
			t.Errorf("unexpected path: %q", gotPath)
		}
		if gotAuth != "Bearer test-token" {
			t.Errorf("expected Authorization Bearer test-token, got %q", gotAuth)
		}
	})

	t.Run("absolute URL is used as-is without auth headers", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			w.Write([]byte("signed-payload"))
		}))
		defer srv.Close()

		client := NewAPIClient("https://api.example.test", "", "test-token")
		data, err := client.DownloadFile(context.Background(), srv.URL+"/signed?sig=abc")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(data) != "signed-payload" {
			t.Errorf("unexpected body: %q", string(data))
		}
		if gotAuth != "" {
			t.Errorf("expected no Authorization header on signed URL, got %q", gotAuth)
		}
	})

	t.Run("relative URL with empty BaseURL returns a helpful error", func(t *testing.T) {
		client := NewAPIClient("", "", "test-token")
		_, err := client.DownloadFile(context.Background(), "/uploads/x.md")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("non-2xx status returns an error with the response body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "not found")
		}))
		defer srv.Close()

		client := NewAPIClient(srv.URL, "", "test-token")
		_, err := client.DownloadFile(context.Background(), "/uploads/missing")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestUploadFileWithURL(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "multipart/form-data") {
				t.Errorf("expected multipart content-type, got %s", ct)
			}

			file, header, err := r.FormFile("file")
			if err != nil {
				t.Fatalf("missing file field: %v", err)
			}
			defer file.Close()

			data, _ := io.ReadAll(file)
			if string(data) != "hello" {
				t.Errorf("unexpected file data: %q", string(data))
			}
			if header.Filename != "test.txt" {
				t.Errorf("unexpected filename: %q", header.Filename)
			}

			// Verify no issue_id or comment_id fields are sent.
			if r.FormValue("issue_id") != "" {
				t.Errorf("unexpected issue_id field")
			}
			if r.FormValue("comment_id") != "" {
				t.Errorf("unexpected comment_id field")
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(AttachmentResponse{
				ID:        "att-123",
				URL:       "https://cdn.example.com/file.txt",
				Filename:  "test.txt",
				SizeBytes: 5,
			})
		}))
		defer srv.Close()

		client := NewAPIClient(srv.URL, "ws-1", "test-token")
		id, url, err := client.UploadFileWithURL(context.Background(), []byte("hello"), "test.txt")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "att-123" {
			t.Errorf("expected id att-123, got %s", id)
		}
		if url != "https://cdn.example.com/file.txt" {
			t.Errorf("expected url https://cdn.example.com/file.txt, got %s", url)
		}
	})

	t.Run("error status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, "bad request")
		}))
		defer srv.Close()

		client := NewAPIClient(srv.URL, "", "")
		_, _, err := client.UploadFileWithURL(context.Background(), []byte("x"), "x.txt")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) {
			t.Fatalf("expected *HTTPError, got %T: %v", err, err)
		}
		if httpErr.StatusCode != 400 {
			t.Errorf("expected status 400, got %d", httpErr.StatusCode)
		}
	})

	t.Run("missing id in response succeeds (fallback path)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"url": "https://example.com"})
		}))
		defer srv.Close()

		client := NewAPIClient(srv.URL, "", "")
		id, url, err := client.UploadFileWithURL(context.Background(), []byte("x"), "x.txt")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "" {
			t.Errorf("expected empty id, got %s", id)
		}
		if url != "https://example.com" {
			t.Errorf("expected url https://example.com, got %s", url)
		}
	})

	t.Run("workspace header sent", func(t *testing.T) {
		var gotWorkspace string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotWorkspace = r.Header.Get("X-Workspace-ID")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(AttachmentResponse{ID: "att-1", URL: "https://example.com"})
		}))
		defer srv.Close()

		client := NewAPIClient(srv.URL, "ws-abc", "test-token")
		_, _, err := client.UploadFileWithURL(context.Background(), []byte("x"), "x.txt")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotWorkspace != "ws-abc" {
			t.Errorf("expected X-Workspace-ID ws-abc, got %s", gotWorkspace)
		}
	})

	t.Run("missing url in response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(AttachmentResponse{ID: "att-123"})
		}))
		defer srv.Close()

		client := NewAPIClient(srv.URL, "", "")
		_, _, err := client.UploadFileWithURL(context.Background(), []byte("x"), "x.txt")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "missing attachment url") {
			t.Errorf("unexpected error message: %s", err.Error())
		}
	})
}

// TestCFAccessHeaders_EnvOverridesConfig covers the Cloudflare Access
// service-token header injection: when both a persisted config default (set
// via SetCFAccessDefaults) and env vars are present, the env vars must win
// on the wire — matching the convention used by `cloudflared access curl` and
// letting a one-off shell override beat a saved token without editing config.
// Also verifies that a partial pair (only ID or only Secret) is dropped
// entirely; CF Access rejects a request presenting only one header, so
// sending half a pair would just yield a confusing 401.
func TestCFAccessHeaders_EnvOverridesConfig(t *testing.T) {
	t.Cleanup(func() { SetCFAccessDefaults("", "") })

	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	SetCFAccessDefaults("cfg-id", "cfg-secret")

	t.Run("config-only fallback when env unset", func(t *testing.T) {
		t.Setenv("CF_ACCESS_CLIENT_ID", "")
		t.Setenv("CF_ACCESS_CLIENT_SECRET", "")

		client := NewAPIClient(srv.URL, "", "")
		if err := client.GetJSON(context.Background(), "/", nil); err != nil {
			t.Fatalf("GetJSON: %v", err)
		}
		if got.Get("CF-Access-Client-Id") != "cfg-id" {
			t.Errorf("CF-Access-Client-Id: got %q, want cfg-id", got.Get("CF-Access-Client-Id"))
		}
		if got.Get("CF-Access-Client-Secret") != "cfg-secret" {
			t.Errorf("CF-Access-Client-Secret: got %q, want cfg-secret", got.Get("CF-Access-Client-Secret"))
		}
	})

	t.Run("env wins when both set", func(t *testing.T) {
		t.Setenv("CF_ACCESS_CLIENT_ID", "env-id")
		t.Setenv("CF_ACCESS_CLIENT_SECRET", "env-secret")

		client := NewAPIClient(srv.URL, "", "")
		if err := client.GetJSON(context.Background(), "/", nil); err != nil {
			t.Fatalf("GetJSON: %v", err)
		}
		if got.Get("CF-Access-Client-Id") != "env-id" {
			t.Errorf("CF-Access-Client-Id: got %q, want env-id", got.Get("CF-Access-Client-Id"))
		}
		if got.Get("CF-Access-Client-Secret") != "env-secret" {
			t.Errorf("CF-Access-Client-Secret: got %q, want env-secret", got.Get("CF-Access-Client-Secret"))
		}
	})

	t.Run("partial env pair is ignored, config still applies", func(t *testing.T) {
		t.Setenv("CF_ACCESS_CLIENT_ID", "env-id-only")
		t.Setenv("CF_ACCESS_CLIENT_SECRET", "")

		client := NewAPIClient(srv.URL, "", "")
		if err := client.GetJSON(context.Background(), "/", nil); err != nil {
			t.Fatalf("GetJSON: %v", err)
		}
		if got.Get("CF-Access-Client-Id") != "cfg-id" {
			t.Errorf("CF-Access-Client-Id: got %q, want cfg-id (env pair incomplete)", got.Get("CF-Access-Client-Id"))
		}
	})

	t.Run("no headers when neither env nor config set", func(t *testing.T) {
		t.Setenv("CF_ACCESS_CLIENT_ID", "")
		t.Setenv("CF_ACCESS_CLIENT_SECRET", "")
		SetCFAccessDefaults("", "")

		client := NewAPIClient(srv.URL, "", "")
		if err := client.GetJSON(context.Background(), "/", nil); err != nil {
			t.Fatalf("GetJSON: %v", err)
		}
		if v := got.Get("CF-Access-Client-Id"); v != "" {
			t.Errorf("CF-Access-Client-Id: got %q, want empty", v)
		}
		if v := got.Get("CF-Access-Client-Secret"); v != "" {
			t.Errorf("CF-Access-Client-Secret: got %q, want empty", v)
		}
	})
}

// TestLoadCLIConfigForProfile_PromotesCFAccessDefaults verifies the loader
// side-effect that ties config to the http client: loading a config file
// with cf_access_client_id / cf_access_client_secret must push those into
// the package defaults so subsequent APIClient requests carry the headers
// without each call site having to plumb them through. Critical for the
// daemon, which loads config once at startup and then relies on setHeaders
// to attach CF Access credentials to every long-poll request.
func TestLoadCLIConfigForProfile_PromotesCFAccessDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CF_ACCESS_CLIENT_ID", "")
	t.Setenv("CF_ACCESS_CLIENT_SECRET", "")
	t.Cleanup(func() { SetCFAccessDefaults("", "") })
	SetCFAccessDefaults("", "")

	seed := CLIConfig{
		ServerURL:            "https://api.example",
		AppURL:               "https://app.example",
		CFAccessClientID:     "persisted-id",
		CFAccessClientSecret: "persisted-secret",
	}
	if err := SaveCLIConfig(seed); err != nil {
		t.Fatalf("save seed config: %v", err)
	}

	if _, err := LoadCLIConfig(); err != nil {
		t.Fatalf("load config: %v", err)
	}

	id, secret := cfAccessHeaders()
	if id != "persisted-id" || secret != "persisted-secret" {
		t.Fatalf("cfAccessHeaders after load = (%q, %q); want persisted values", id, secret)
	}
}

func TestNormalizeGOOS(t *testing.T) {
	cases := map[string]string{
		"darwin":  "macos",
		"windows": "windows",
		"linux":   "linux",
		"freebsd": "freebsd",
	}
	for in, want := range cases {
		if got := normalizeGOOS(in); got != want {
			t.Errorf("normalizeGOOS(%q) = %q, want %q", in, got, want)
		}
	}
}
