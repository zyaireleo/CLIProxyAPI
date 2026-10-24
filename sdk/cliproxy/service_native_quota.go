package cliproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/nativequota"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
)

type nativeQuotaPersistence struct {
	path   string
	cancel context.CancelFunc
	done   chan struct{}
}

// The path is separate from auth-dir so credential watchers never ingest snapshots.
func nativeQuotaSnapshotPath(configPath, authDir string) (string, error) {
	if configPath == "" || authDir == "" {
		return "", nil
	}
	absoluteConfig, errConfig := filepath.Abs(configPath)
	if errConfig != nil {
		return "", errConfig
	}
	absoluteAuth, errAuth := filepath.Abs(authDir)
	if errAuth != nil {
		return "", errAuth
	}
	digest := sha256.Sum256([]byte(filepath.Clean(absoluteConfig) + "\x00" + filepath.Clean(absoluteAuth)))
	return filepath.Join(filepath.Dir(absoluteConfig), ".native-quota-state", hex.EncodeToString(digest[:12]), "snapshot.json"), nil
}

func (s *Service) configureNativeQuotaPersistence(ctx context.Context, cfg *config.Config) error {
	if s == nil || s.coreManager == nil || cfg == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runtime, errConfig := cfg.NativeQuota.RuntimeConfig()
	if errConfig != nil {
		return errConfig
	}
	if runtime.Mode == nativequota.ModeOff || cfg.Home.Enabled {
		return s.stopNativeQuotaPersistence(ctx)
	}
	path, errPath := nativeQuotaSnapshotPath(s.configPath, cfg.AuthDir)
	if errPath != nil || path == "" {
		return errPath
	}
	s.nativeQuotaPersistenceMu.Lock()
	defer s.nativeQuotaPersistenceMu.Unlock()
	if current := s.nativeQuotaPersistence; current != nil {
		if current.path == path {
			return nil
		}
		current.cancel()
		select {
		case <-current.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		s.nativeQuotaPersistence = nil
	}
	if info, errStat := os.Lstat(path); errStat == nil && !info.Mode().IsRegular() {
		return errors.New("native quota snapshot target is not a regular file")
	} else if errStat != nil && !errors.Is(errStat, os.ErrNotExist) {
		return errStat
	}
	engine := s.coreManager.NativeQuota()
	if errLoad := engine.LoadSnapshot(path); errLoad != nil && !errors.Is(errLoad, os.ErrNotExist) {
		log.Warn("native quota snapshot was not restored; fresh quota evidence is required")
	}
	if errDir := os.MkdirAll(filepath.Dir(path), 0o700); errDir != nil {
		return errDir
	}
	runCtx, cancel := context.WithCancel(context.Background())
	worker := &nativeQuotaPersistence{path: path, cancel: cancel, done: make(chan struct{})}
	s.nativeQuotaPersistence = worker
	go runNativeQuotaPersistence(runCtx, engine, path, worker.done, time.Minute)
	return nil
}

func (s *Service) stopNativeQuotaPersistence(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.nativeQuotaPersistenceMu.Lock()
	defer s.nativeQuotaPersistenceMu.Unlock()
	worker := s.nativeQuotaPersistence
	if worker == nil {
		return nil
	}
	worker.cancel()
	select {
	case <-worker.done:
		s.nativeQuotaPersistence = nil
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func runNativeQuotaPersistence(ctx context.Context, engine *nativequota.Engine, path string, done chan struct{}, interval time.Duration) {
	defer close(done)
	timer := time.NewTicker(interval)
	defer timer.Stop()
	var last [sha256.Size]byte
	var saved bool
	save := func() {
		raw, errExport := engine.ExportSnapshot()
		if errExport != nil {
			if !errors.Is(errExport, nativequota.ErrModeOff) {
				log.Warn("native quota snapshot export failed")
			}
			return
		}
		hash := sha256.Sum256(raw)
		if saved && hash == last {
			return
		}
		if errSave := engine.SaveSnapshot(path); errSave != nil {
			if !errors.Is(errSave, nativequota.ErrModeOff) {
				log.Warn("native quota snapshot save failed")
			}
			return
		}
		last, saved = hash, true
	}
	for {
		select {
		case <-ctx.Done():
			save()
			return
		case <-timer.C:
			save()
		}
	}
}
