package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// mintFwdToken signs "appID.expUnix" with HMAC-SHA256. Used for the
// one-click open handoff (60s) and the luncur_fwd cookie (4h). The key is
// per-boot random (server.fwdKey) — a restart just makes users re-click
// open, no state to persist.
func mintFwdToken(key []byte, appID int64, exp time.Time) string {
	payload := fmt.Sprintf("%d.%d", appID, exp.Unix())
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

// verifyFwdToken checks signature and expiry, returning the app ID.
func verifyFwdToken(key []byte, token string, now time.Time) (int64, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0, false
	}
	want, err := hex.DecodeString(parts[2])
	if err != nil {
		return 0, false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(mac.Sum(nil), want) {
		return 0, false
	}
	appID, err1 := strconv.ParseInt(parts[0], 10, 64)
	expUnix, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || !now.Before(time.Unix(expUnix, 0)) {
		return 0, false
	}
	return appID, true
}
