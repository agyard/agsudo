package helper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/agyard/agsudo/internal/api"
	"github.com/agyard/agsudo/internal/store"
)

const SecurePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

type Config struct {
	AllowedUIDs       []int  `json:"allowed_uids"`
	ReviewerUIDs      []int  `json:"reviewer_uids"`
	StateDir          string `json:"state_dir"`
	SecurePath        string `json:"secure_path"`
	DefaultTTLSeconds int64  `json:"default_ttl_seconds"`
	MaxTTLSeconds     int64  `json:"max_ttl_seconds"`
	Dev               bool   `json:"-"`
}

func defaults() Config {
	return Config{StateDir: "/var/lib/agsudo", SecurePath: SecurePath, DefaultTTLSeconds: 600, MaxTTLSeconds: 86400}
}
func (c Config) access(uid int) (store.Access, error) {
	a := store.Access{UID: uid, Reviewer: uid == 0 || slices.Contains(c.ReviewerUIDs, uid)}
	if uid != 0 && !a.Reviewer && !slices.Contains(c.AllowedUIDs, uid) {
		return a, api.Err("forbidden", "UID %d is not configured to use AG-sudo", uid)
	}
	return a, nil
}
func (c Config) canSubmit(uid int) bool { return uid == 0 || slices.Contains(c.AllowedUIDs, uid) }

// decodeOne reads exactly one JSON value of at most limit bytes, rejecting
// unknown fields and trailing data.
func decodeOne(r io.Reader, limit int, v any) error {
	b, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err == nil && len(b) > limit {
		err = fmt.Errorf("larger than %d bytes", limit)
	}
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(v); err == nil && d.Decode(new(any)) != io.EOF {
		err = errors.New("trailing data")
	}
	return err
}
