package vfs

import (
	"encoding/json"
	"errors"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/localfile"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type RecoveryRecord struct {
	Account string         `json:"account,omitempty"`
	Server  string         `json:"server,omitempty"`
	Entry   anyshare.Entry `json:"entry"`
	Parent  string         `json:"parent"`
	Data    string         `json:"data"`
	Updated time.Time      `json:"updated"`
}

func RecoveryFiles(dir string) ([]RecoveryRecord, error) {
	var result []RecoveryRecord
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var r RecoveryRecord
		if err = json.Unmarshal(b, &r); err != nil {
			return err
		}
		if !localfile.ValidName(r.Data) {
			return errors.New("invalid recovery data path")
		}
		r.Data = filepath.Join(filepath.Dir(path), r.Data)
		result = append(result, r)
		return nil
	})
	sort.Slice(result, func(i, j int) bool { return result[i].Data < result[j].Data })
	return result, err
}
