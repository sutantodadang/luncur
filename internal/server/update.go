package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"

	"github.com/sutantodadang/luncur/internal/store"
)

// serverImageRepo is luncur's own image, published by CI on every tagged
// release. updateServerImage appends ":<version>" to it unless the caller
// supplies an explicit image (e.g. a private mirror or a pre-release build).
const serverImageRepo = "ghcr.io/sutantodadang/luncur"

// validVersionTag guards against feeding an arbitrary string into an image
// reference: must look like a release tag (v-prefixed, semver-ish).
var validVersionTag = regexp.MustCompile(`^v[0-9][0-9A-Za-z._-]*$`)

// errInvalidUpdateRequest is updateServerImage's sentinel for a bad
// version/image combination — callers map it to 400.
var errInvalidUpdateRequest = errors.New("version must look like a release tag (e.g. v0.4.2), or pass image explicitly")

// updateServerImage is handleSystemUpdate's and handleUISettingsUpdate's
// shared core: resolve the target image and patch luncur's own Deployment
// to it. image, when set, wins as-is (private mirrors, pre-release builds);
// otherwise version must be a valid release tag and is appended to
// serverImageRepo. Returns the resolved image.
func (s *server) updateServerImage(ctx context.Context, version, image string) (string, error) {
	if image == "" {
		if !validVersionTag.MatchString(version) {
			return "", errInvalidUpdateRequest
		}
		image = serverImageRepo + ":" + version
	}
	if err := s.kube.SetDeploymentImage(ctx, s.systemNamespace, "luncur", "luncur", image); err != nil {
		return "", err
	}
	return image, nil
}

// handleSystemUpdate bumps luncur's own server Deployment to a new image —
// no fresh reinstall needed. The rolling update takes it from there.
func (s *server) handleSystemUpdate(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.requireKube(w) {
		return
	}

	var req struct {
		Version string `json:"version"`
		Image   string `json:"image"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	img, err := s.updateServerImage(r.Context(), req.Version, req.Image)
	if err != nil {
		if errors.Is(err, errInvalidUpdateRequest) {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		log.Printf("system update: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"image": img})
}
