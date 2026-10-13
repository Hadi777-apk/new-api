package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

const imageURLLifetime = int64(24 * time.Hour / time.Second)

// ErrImageURLExpired distinguishes an expired link from an invalid or missing one.
var ErrImageURLExpired = errors.New("image URL expired")

func imageURLStorageDir() string {
	return filepath.Join(DrawingStorageDir(), "api-images")
}

// FormatImageURLResponse stores the already validated, normalized image bytes
// without re-encoding them. The random filename is a limited-lifetime capability;
// it never contains an API key, user ID, or upstream download URL.
func FormatImageURLResponse(ctx context.Context, body []byte, baseURL string) (result []byte, err error) {
	base, parseErr := url.Parse(strings.TrimSpace(baseURL))
	if parseErr != nil || base == nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Opaque != "" {
		return nil, errors.New("image URL base address is unavailable")
	}
	var response map[string]json.RawMessage
	var items []map[string]json.RawMessage
	if common.Unmarshal(body, &response) != nil || response == nil || common.Unmarshal(response["data"], &items) != nil || len(items) == 0 || len(items) > dto.MaxImageN {
		return nil, errors.New("invalid image response")
	}
	created := make([]string, 0, len(items))
	defer func() {
		if err != nil {
			for _, path := range created {
				_ = os.Remove(path)
			}
		}
	}()
	expiresAt := time.Now().Unix() + imageURLLifetime
	for index, item := range items {
		if err = ctx.Err(); err != nil {
			return nil, errors.New("image URL delivery cancelled or timed out")
		}
		var encoded string
		if item == nil || common.Unmarshal(item["b64_json"], &encoded) != nil || encoded == "" {
			return nil, fmt.Errorf("image %d: image Base64 is unavailable", index+1)
		}
		data, decodeErr := io.ReadAll(io.LimitReader(base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded)), MaxDrawingBytes+1))
		if decodeErr != nil || len(data) == 0 || int64(len(data)) > MaxDrawingBytes {
			return nil, fmt.Errorf("image %d: invalid or oversized image Base64", index+1)
		}
		_, format, configErr := image.DecodeConfig(bytes.NewReader(data))
		extension := imageURLFileExtension(format)
		if configErr != nil || extension == "" {
			return nil, fmt.Errorf("image %d: unsupported image format", index+1)
		}
		var token [32]byte
		if _, err = rand.Read(token[:]); err != nil {
			return nil, errors.New("unable to create image URL")
		}
		name := strconv.FormatInt(expiresAt, 10) + "-" + hex.EncodeToString(token[:]) + "." + extension
		path := filepath.Join(imageURLStorageDir(), name)
		if err = writeImageURLFile(path, data); err != nil {
			return nil, errors.New("unable to store image URL result")
		}
		created = append(created, path)
		location := *base
		location.Path = strings.TrimRight(base.Path, "/") + "/v1/images/files/" + name
		location.RawPath = ""
		item["url"], err = common.Marshal(location.String())
		if err != nil {
			return nil, errors.New("unable to encode image URL")
		}
		item["expires_at"], _ = common.Marshal(expiresAt)
		delete(item, "b64_json")
	}
	if err = ctx.Err(); err != nil {
		return nil, errors.New("image URL delivery cancelled or timed out")
	}
	response["data"], err = common.Marshal(items)
	if err != nil {
		return nil, errors.New("unable to encode image URL response")
	}
	result, err = common.Marshal(response)
	if err == nil && ctx.Err() != nil {
		return nil, errors.New("image URL delivery cancelled or timed out")
	}
	return result, err
}

func imageURLFileExtension(format string) string {
	switch format {
	case "jpeg":
		return "jpg"
	case "png", "gif", "webp":
		return format
	default:
		return ""
	}
}

func writeImageURLFile(path string, data []byte) error {
	dir := imageURLStorageDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, "write-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func imageURLFileExpiry(name string) (int64, error) {
	if filepath.Base(name) != name || strings.ContainsAny(name, "/\\") {
		return 0, errors.New("invalid image URL filename")
	}
	separator := strings.IndexByte(name, '-')
	if separator < 1 {
		return 0, errors.New("invalid image URL filename")
	}
	expiresAt, err := strconv.ParseInt(name[:separator], 10, 64)
	if err != nil || expiresAt <= 0 || strconv.FormatInt(expiresAt, 10) != name[:separator] {
		return 0, errors.New("invalid image URL filename")
	}
	extension := filepath.Ext(name)
	if extension != ".png" && extension != ".jpg" && extension != ".gif" && extension != ".webp" {
		return 0, errors.New("invalid image URL filename")
	}
	encoded := strings.TrimSuffix(name[separator+1:], extension)
	token, err := hex.DecodeString(encoded)
	if err != nil || len(token) != 32 || hex.EncodeToString(token) != encoded {
		return 0, errors.New("invalid image URL filename")
	}
	return expiresAt, nil
}

// OpenImageURLFile accepts only the random capability filenames issued above.
// Expiry is checked on every download independently of periodic disk cleanup.
func OpenImageURLFile(name string, now int64) (*os.File, error) {
	expiresAt, err := imageURLFileExpiry(name)
	if err != nil {
		return nil, err
	}
	if expiresAt <= now {
		return nil, ErrImageURLExpired
	}
	path := filepath.Join(imageURLStorageDir(), name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("image URL file unavailable")
	}
	return os.Open(path)
}

// CleanupImageURLFiles shares the private drawing volume but keeps an independent
// 24-hour retention, including abandoned temporary writes after a process crash.
func CleanupImageURLFiles(now int64) error {
	dir := imageURLStorageDir()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		expired := false
		if expiresAt, parseErr := imageURLFileExpiry(entry.Name()); parseErr == nil {
			expired = expiresAt <= now
		} else if strings.HasPrefix(entry.Name(), "write-") {
			info, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}
			expired = info.ModTime().Unix() <= now-imageURLLifetime
		}
		if expired {
			if err = os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}
