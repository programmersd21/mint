package downloads

import (
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Status string

const (
	StatusQueued      Status = "queued"
	StatusPreparing   Status = "preparing"
	StatusDownloading Status = "downloading"
	StatusVerifying   Status = "verifying"
	StatusCompleted   Status = "completed"
	StatusFailed      Status = "failed"
	StatusCancelled   Status = "cancelled"
)

type HashType string

const (
	HashSHA1   HashType = "sha1"
	HashSHA512 HashType = "sha512"
)

type HashInfo struct {
	Type  HashType
	Value string
}

type Item struct {
	ID             int
	ProjectID      string
	ProjectTitle   string
	VersionID      string
	VersionNumber  string
	FileURL        string
	Filename       string
	TotalSize      int64
	DownloadedSize int64
	Status         Status
	Error          string
	Progress       float64
	Speed          float64
	ETA            time.Duration
	Hash           *HashInfo
	Verified       bool
	CreatedAt      time.Time
	StartedAt      *time.Time
	CompletedAt    *time.Time

	destPath string
	cancel   context.CancelFunc
	mu       sync.Mutex
}

func (item *Item) DestPath() string {
	item.mu.Lock()
	defer item.mu.Unlock()
	return item.destPath
}

type Persister interface {
	SaveDownload(projectName, projectID, versionID, versionNumber, filename, filePath, url string, size int64, status string, isMRPack bool) (int64, error)
	UpdateDownloadStatus(id int64, status string) error
	ListDownloads() ([]DownloadRecord, error)
	DeleteDownload(id int64) error
	DeleteAllDownloads() error
	MarkInstalled(downloadID int64) error
}

type DownloadRecord struct {
	ID          int64
	ProjectName string
	ProjectID   string
	VersionID   string
	VersionNum  string
	Filename    string
	FilePath    string
	URL         string
	Size        int64
	Status      string
	IsMRPack    bool
	CreatedAt   string
	InstalledAt *string
}

type Manager struct {
	mu           sync.RWMutex
	items        []*Item
	nextID       atomic.Int64
	active       atomic.Int64
	maxWorkers   int
	client       *http.Client
	downloadsDir string
	workChan     chan *Item
	persister    Persister
	OnComplete   func(item *Item)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewManager(downloadsDir string, maxWorkers int, persister Persister) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	if maxWorkers < 1 {
		maxWorkers = 1
	}

	m := &Manager{
		items:        make([]*Item, 0),
		maxWorkers:   maxWorkers,
		client:       &http.Client{Timeout: 0},
		downloadsDir: downloadsDir,
		workChan:     make(chan *Item, 100),
		persister:    persister,
		ctx:          ctx,
		cancel:       cancel,
	}

	m.restore()

	_ = os.MkdirAll(downloadsDir, 0755)

	for range maxWorkers {
		m.wg.Add(1)
		go m.worker()
	}

	return m
}

func (m *Manager) Close() {
	m.cancel()
	m.wg.Wait()
}

func (m *Manager) Dir() string {
	return m.downloadsDir
}

func (m *Manager) SetPersister(p Persister) {
	m.persister = p
}

func (m *Manager) Persist() Persister {
	return m.persister
}

func (m *Manager) restore() {
	if m.persister == nil {
		return
	}
	records, err := m.persister.ListDownloads()
	if err != nil || len(records) == 0 {
		return
	}
	for _, r := range records {
		status := Status(r.Status)
		switch status {
		case StatusDownloading, StatusQueued, StatusPreparing, StatusVerifying:
			status = StatusFailed
			_ = m.persister.UpdateDownloadStatus(r.ID, string(status))
		}
		item := &Item{
			ID:            int(r.ID),
			ProjectID:     r.ProjectID,
			ProjectTitle:  r.ProjectName,
			VersionID:     r.VersionID,
			VersionNumber: r.VersionNum,
			FileURL:       r.URL,
			Filename:      r.Filename,
			TotalSize:     r.Size,
			Status:        status,
			destPath:      r.FilePath,
		}
		m.items = append(m.items, item)
		if int64(m.nextID.Load()) <= r.ID {
			m.nextID.Store(r.ID + 1)
		}
	}
}

func (m *Manager) Enqueue(projectID, projectTitle, versionID, versionNumber, fileURL, filename string, totalSize int64, hash *HashInfo) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	filename = safeFilename(filename)
	id := int(m.nextID.Add(1))
	destPath := filepath.Join(m.downloadsDir, filename)
	if m.persister != nil {
		if savedID, err := m.persister.SaveDownload(projectTitle, projectID, versionID, versionNumber, filename, destPath, fileURL, totalSize, string(StatusQueued), strings.HasSuffix(filename, ".mrpack")); err == nil {
			id = int(savedID)
			for {
				current := m.nextID.Load()
				if current >= savedID {
					break
				}
				if m.nextID.CompareAndSwap(current, savedID) {
					break
				}
			}
		}
	}

	item := &Item{
		ID:            id,
		ProjectID:     projectID,
		ProjectTitle:  projectTitle,
		VersionID:     versionID,
		VersionNumber: versionNumber,
		FileURL:       fileURL,
		Filename:      filename,
		TotalSize:     totalSize,
		Status:        StatusQueued,
		Hash:          hash,
		CreatedAt:     time.Now(),
		destPath:      destPath,
	}

	m.items = append(m.items, item)

	select {
	case m.workChan <- item:
	default:
		go func() { m.workChan <- item }()
	}

	return id
}

func (m *Manager) Cancel(id int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, item := range m.items {
		if item.ID == id {
			item.mu.Lock()
			canCancel := item.Status == StatusQueued || item.Status == StatusPreparing || item.Status == StatusDownloading
			if canCancel {
				if item.cancel != nil {
					item.cancel()
				}
				item.Status = StatusCancelled
				m.persistStatus(item.ID, item.Status)
			}
			item.mu.Unlock()
			return canCancel
		}
	}
	return false
}

func (m *Manager) Retry(id int) bool {
	m.mu.Lock()
	var item *Item
	for _, it := range m.items {
		if it.ID == id {
			it.mu.Lock()
			if it.Status == StatusFailed {
				item = it
				it.mu.Unlock()
				break
			}
			it.mu.Unlock()
		}
	}
	m.mu.Unlock()

	if item == nil {
		return false
	}

	item.mu.Lock()
	item.Status = StatusQueued
	item.Error = ""
	item.DownloadedSize = 0
	item.Progress = 0
	item.Speed = 0
	item.ETA = 0
	m.persistStatus(item.ID, item.Status)
	item.Verified = false
	item.StartedAt = nil
	item.CompletedAt = nil
	item.mu.Unlock()

	select {
	case m.workChan <- item:
	default:
		go func() { m.workChan <- item }()
	}

	return true
}

func (m *Manager) ClearAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, item := range m.items {
		if item.cancel != nil {
			item.cancel()
		}
		item.mu.Lock()
		item.Status = StatusCancelled
		item.mu.Unlock()
	}
	m.items = nil
	m.active.Store(0)
	if m.persister != nil {
		_ = m.persister.DeleteAllDownloads()
	}
}

func (m *Manager) List() []*Item {
	m.mu.RLock()
	defer m.mu.RUnlock()

	items := make([]*Item, len(m.items))
	copy(items, m.items)
	return items
}

func (m *Manager) ActiveCount() int64 {
	return m.active.Load()
}

func (m *Manager) worker() {
	defer m.wg.Done()

	for {
		select {
		case <-m.ctx.Done():
			return
		case item := <-m.workChan:
			m.process(item)
		}
	}
}

func (m *Manager) process(item *Item) {
	item.mu.Lock()
	if item.Status != StatusQueued {
		item.mu.Unlock()
		return
	}
	item.Status = StatusPreparing
	m.active.Add(1)
	ctx, cancel := context.WithCancel(m.ctx)
	item.cancel = cancel
	now := time.Now()
	item.StartedAt = &now
	item.mu.Unlock()

	item.mu.Lock()
	item.Status = StatusDownloading
	item.mu.Unlock()

	err := m.download(ctx, item)

	if err != nil {
		item.mu.Lock()
		item.cancel = nil
		m.active.Add(-1)
		if ctx.Err() != nil {
			if item.Status != StatusCancelled {
				item.Status = StatusFailed
				item.Error = "cancelled"
			}
		} else {
			item.Status = StatusFailed
			item.Error = err.Error()
		}
		m.persistStatus(item.ID, item.Status)
		item.mu.Unlock()
		return
	}

	item.mu.Lock()
	item.Status = StatusVerifying
	item.mu.Unlock()

	verified := m.verify(item)

	item.mu.Lock()
	item.cancel = nil
	m.active.Add(-1)
	item.Verified = verified
	var wasCompleted bool
	if verified {
		item.Status = StatusCompleted
		item.Progress = 1.0
		now := time.Now()
		item.CompletedAt = &now
		wasCompleted = true
	} else if item.Status != StatusCancelled {
		item.Status = StatusFailed
		item.Error = "hash mismatch"
	}
	m.persistStatus(item.ID, item.Status)
	item.mu.Unlock()

	if wasCompleted && m.OnComplete != nil {
		m.OnComplete(item)
	}
}

func (m *Manager) persistStatus(id int, status Status) {
	if m.persister == nil {
		return
	}
	_ = m.persister.UpdateDownloadStatus(int64(id), string(status))
}

func (m *Manager) download(ctx context.Context, item *Item) error {
	filename := safeFilename(item.Filename)
	destPath := filepath.Join(m.downloadsDir, filename)
	tmpPath := destPath + ".tmp"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, item.FileURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("User-Agent", "Mint/0.1.0 (terminal-modrinth-client)")

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	if item.TotalSize == 0 && resp.ContentLength > 0 {
		item.mu.Lock()
		item.TotalSize = resp.ContentLength
		item.mu.Unlock()
	}

	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}

	buf := make([]byte, 32*1024)
	var written int64
	startTime := time.Now()
	lastUpdate := startTime
	lastWritten := int64(0)

	for {
		select {
		case <-ctx.Done():
			f.Close()
			os.Remove(tmpPath)
			return ctx.Err()
		default:
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			_, writeErr := f.Write(buf[:n])
			if writeErr != nil {
				f.Close()
				os.Remove(tmpPath)
				return fmt.Errorf("write file: %w", writeErr)
			}
			written += int64(n)

			now := time.Now()
			elapsed := now.Sub(startTime).Seconds()
			if elapsed > 0 {
				item.mu.Lock()
				item.DownloadedSize = written
				if item.TotalSize > 0 {
					item.Progress = float64(written) / float64(item.TotalSize)
				}
				if now.Sub(lastUpdate) > 200*time.Millisecond {
					dt := now.Sub(lastUpdate).Seconds()
					if dt > 0 {
						instantSpeed := float64(written-lastWritten) / dt
						item.Speed = instantSpeed
						if item.Speed > 0 && item.TotalSize > 0 {
							remaining := float64(item.TotalSize - written)
							item.ETA = time.Duration(remaining/item.Speed) * time.Second
						}
					}
					lastUpdate = now
					lastWritten = written
				}
				item.mu.Unlock()
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			f.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("read response: %w", readErr)
		}
	}

	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close file: %w", err)
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("rename file: %w", err)
	}

	return nil
}

func (m *Manager) verify(item *Item) bool {
	if item.Hash == nil || item.Hash.Value == "" {
		return true
	}

	destPath := filepath.Join(m.downloadsDir, safeFilename(item.Filename))
	f, err := os.Open(destPath)
	if err != nil {
		return false
	}
	defer f.Close()

	var h hash.Hash
	switch item.Hash.Type {
	case HashSHA1:
		h = sha1.New()
	case HashSHA512:
		h = sha512.New()
	default:
		return false
	}

	if _, err := io.Copy(h, f); err != nil {
		return false
	}

	computed := hex.EncodeToString(h.Sum(nil))
	return computed == item.Hash.Value
}

func safeFilename(filename string) string {
	name := filepath.Base(filename)
	if name == "." || name == ".." || name == string(filepath.Separator) || name == "" {
		return "download"
	}
	return name
}
