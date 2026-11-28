package router

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/phongnd2802/trelis/internal/apidocs"
)

func TestSwaggerRoutesAnswerWithoutAKeyWhenEnabled(t *testing.T) {
	h := newRouterWith(keyring{}, true)

	rec := get(t, h, "/swagger/index.html", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /swagger/index.html without a key = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("index.html Content-Type = %q, want HTML", ct)
	}

	rec = get(t, h, "/swagger/doc.json", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /swagger/doc.json without a key = %d, want 200", rec.Code)
	}
	var spec struct {
		Swagger string `json:"swagger"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("doc.json is not JSON: %v", err)
	}
	if spec.Swagger != "2.0" {
		t.Errorf(`doc.json "swagger" = %q, want "2.0"`, spec.Swagger)
	}
}

func TestSwaggerIsHiddenBehindTheKeyWhenDisabled(t *testing.T) {
	h := newRouterWith(keyring{"trelis_alice": alice}, false)

	for _, path := range []string{"/swagger/index.html", "/swagger/doc.json"} {
		if rec := get(t, h, path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a key = %d, want 401", path, rec.Code)
		}
		if rec := get(t, h, path, "Bearer trelis_alice"); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s with a key = %d, want 404", path, rec.Code)
		}
	}
}

// TestTheSpecDocumentsEveryRoute compares the routes chi serves with the
// method+path pairs in the committed spec, in both directions: an
// undocumented route and a stale spec entry both fail.
func TestTheSpecDocumentsEveryRoute(t *testing.T) {
	// nil handlers are fine: New takes their method values, it never calls them.
	routes := New(Readiness{}, keyring{}, nil, nil, nil, nil, nil, nil, false)
	var served []string
	err := chi.Walk(routes.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasPrefix(route, "/swagger/") {
			return nil
		}
		served = append(served, strings.ToUpper(method)+" "+route)
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}

	doc := apidocs.SwaggerInfo.ReadDoc()
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal([]byte(doc), &spec); err != nil {
		t.Fatalf("generated spec is not JSON: %v", err)
	}
	var documented []string
	for path, operations := range spec.Paths {
		for method := range operations {
			documented = append(documented, strings.ToUpper(method)+" "+path)
		}
	}

	sort.Strings(served)
	sort.Strings(documented)
	for _, route := range served {
		if !contains(documented, route) {
			t.Errorf("%s is served but missing from the spec (add swag annotations and run make swagger)", route)
		}
	}
	for _, route := range documented {
		if !contains(served, route) {
			t.Errorf("%s is in the spec but not served (stale spec: run make swagger)", route)
		}
	}
}

func contains(sorted []string, want string) bool {
	i := sort.SearchStrings(sorted, want)
	return i < len(sorted) && sorted[i] == want
}
