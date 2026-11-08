package httpapi

import (
	"net/http"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

func TestVolumeRoutesAndTheCreateField(t *testing.T) {
	fake := runtimetest.NewFake()
	fake.Volumes = true
	srv, key := newFakeRuntimeServer(t, fake)

	code, vol := do(t, "POST", srv.URL+"/v1/volumes", key, `{"name":"data","size_mb":1024}`)
	if code != http.StatusCreated {
		t.Fatalf("create volume = %d %v", code, vol)
	}
	id, _ := vol["id"].(string)
	if at, _ := vol["created_at"].(float64); at == 0 {
		t.Errorf("create volume answered created_at %v, want the time it was made", vol["created_at"])
	}

	code, sb := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base","volumes":[{"id":"`+id+`","path":"/data"}]}`)
	if code != http.StatusCreated {
		t.Fatalf("create with volume = %d %v", code, sb)
	}
	if spec, _ := fake.LastBootSpec(); len(spec.Volumes) != 1 || spec.Volumes[0].Path != "/data" {
		t.Errorf("boot got volumes %+v", spec.Volumes)
	}
	if code, _ := do(t, "GET", srv.URL+"/v1/volumes/"+id, key, ""); code != http.StatusOK {
		t.Errorf("get volume = %d", code)
	}
	if code, _ := do(t, "DELETE", srv.URL+"/v1/volumes/"+id, key, ""); code != http.StatusConflict {
		t.Errorf("delete while attached = %d, want 409", code)
	}
	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base","volumes":[{"id":"`+id+`","path":"/data"}]}`); code != http.StatusConflict {
		t.Errorf("second sandbox on the volume = %d, want 409", code)
	}
	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base","volumes":[{"id":"vol-nope0000","path":"/data"}]}`); code != http.StatusNotFound {
		t.Errorf("unknown volume = %d, want 404", code)
	}
	if code, _ := do(t, "POST", srv.URL+"/v1/volumes", key, `{"size_mb":1}`); code != http.StatusBadRequest {
		t.Errorf("1 MB volume = %d, want 400", code)
	}

	_, free := do(t, "POST", srv.URL+"/v1/volumes", key, `{"name":"free","size_mb":1024}`)
	freeID, _ := free["id"].(string)
	for _, p := range []string{"/data;reboot", "../x", "/etc", "data"} {
		if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base","volumes":[{"id":"`+freeID+`","path":"`+p+`"}]}`); code != http.StatusBadRequest {
			t.Errorf("mount path %q = %d, want 400", p, code)
		}
	}
}
