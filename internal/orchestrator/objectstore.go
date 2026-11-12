package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

const objectStoreProbeKey = "metadata-probe/object-store-check"

func openObjectStore(o store.ObjectStore) (artifact.StorageProvider, error) {
	return artifact.Open(artifact.KindS3, artifact.Options{S3: artifact.S3Options{
		Endpoint: o.Endpoint, Bucket: o.Bucket, Region: o.Region, AccessKey: o.AccessKey, SecretKey: o.SecretKey,
	}})
}

// probeObjectStore writes, reads and deletes one object, so a key that cannot
// do all three is refused at save rather than at the first backup.
func probeObjectStore(ctx context.Context, blobs artifact.StorageProvider) error {
	want := []byte("pilldrops object store check")
	if err := blobs.Put(ctx, objectStoreProbeKey, bytes.NewReader(want), int64(len(want))); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	rc, err := blobs.Get(ctx, objectStoreProbeKey)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("read: got %d bytes back, want the %d written", len(got), len(want))
	}
	if err := blobs.Delete(ctx, objectStoreProbeKey); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}

// UseSavedObjectStore opens the bucket an operator saved. Call it only when the
// environment names no artifact store: the environment wins.
func (s *Server) UseSavedObjectStore(ctx context.Context, st *store.Store) {
	saved, err := st.GetObjectStore(ctx)
	if err != nil || saved == nil {
		if err != nil {
			s.log().Error("saved object store unreadable", "component", "artifact", "err", err)
		}
		return
	}
	blobs, err := openObjectStore(*saved)
	if err != nil {
		s.log().Error("saved object store unusable", "component", "artifact", "err", err)
		return
	}
	s.SetArtifactStore(blobs)
	s.artifactsSaved.Store(true)
	s.log().Info("artifact store ready", "component", "artifact", "kind", blobs.Name(), "bucket", saved.Bucket, "source", "saved")
}

func (s *Server) objectStoreFromEnv() bool {
	p := s.artifacts.Load()
	return p != nil && *p != nil && !s.artifactsSaved.Load()
}

// objectStoreView never carries the secret key: it is write-only.
func (s *Server) objectStoreView(ctx context.Context, st *store.Store) (map[string]any, error) {
	if s.objectStoreFromEnv() {
		return map[string]any{"configured": true, "source": "env"}, nil
	}
	saved, err := st.GetObjectStore(ctx)
	if err != nil {
		return nil, err
	}
	if saved == nil {
		return map[string]any{"configured": false}, nil
	}
	hint := saved.AccessKey
	if len(hint) > 4 {
		hint = hint[len(hint)-4:]
	}
	return map[string]any{
		"configured": true, "source": "saved",
		"endpoint": saved.Endpoint, "bucket": saved.Bucket, "region": saved.Region, "access_key_hint": hint,
	}, nil
}

func (s *Server) writeObjectStoreView(w http.ResponseWriter, r *http.Request, st *store.Store) {
	view, err := s.objectStoreView(r.Context(), st)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleObjectStoreGet(w http.ResponseWriter, r *http.Request) {
	if st := s.projectStore(w); st != nil {
		s.writeObjectStoreView(w, r, st)
	}
}

// envOwnsObjectStore answers 409 when the environment configured the store: a
// saved one would be ignored here yet used by nodes, splitting the fleet over two buckets.
func (s *Server) envOwnsObjectStore(w http.ResponseWriter) bool {
	if s.objectStoreFromEnv() {
		writeJSONErr(w, http.StatusConflict, "object_store_set_by_env",
			"the artifact store is set by ORCHESTRATOR_ARTIFACT_* environment variables; "+
				"remove them and restart the control plane to manage it here")
		return true
	}
	return false
}

func (s *Server) handleObjectStorePut(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil || s.envOwnsObjectStore(w) {
		return
	}
	var o store.ObjectStore
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	blobs, err := openObjectStore(o)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := probeObjectStore(ctx, blobs); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "object_store_unusable", "the bucket refused the test object: "+err.Error())
		return
	}
	if err := st.SetObjectStore(r.Context(), o); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, store.ErrObjectStoreNeedsKey) {
			code = http.StatusConflict
		}
		writeErr(w, code, err.Error())
		return
	}
	// ponytail: only this replica switches now; other replicas and every node pick it up on restart.
	s.SetArtifactStore(blobs)
	s.artifactsSaved.Store(true)
	s.auditObjectStore(r, st, "object_store.set", o.Bucket)
	s.writeObjectStoreView(w, r, st)
}

func (s *Server) handleObjectStoreDelete(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil || s.envOwnsObjectStore(w) {
		return
	}
	if err := st.DeleteObjectStore(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.artifacts.Store(nil)
	s.artifactsSaved.Store(false)
	s.auditObjectStore(r, st, "object_store.delete", "")
	s.writeObjectStoreView(w, r, st)
}

func (s *Server) auditObjectStore(r *http.Request, st *store.Store, action, bucket string) {
	if err := st.RecordAuditDetail(r.Context(), fastOrg(r), "", action, bucket, ""); err != nil {
		s.log().Warn("audit write failed", "component", "artifact", "action", action, "err", err)
	}
}
