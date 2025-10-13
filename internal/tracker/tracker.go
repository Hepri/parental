//go:build windows

package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type TimeTracker struct {
	dataPath      string
	currentApp    string
	startTime     time.Time
	dailyData     map[string]map[string]int64 // date -> app -> seconds
	mutex         sync.RWMutex
	retentionDays int
}

type TimeData struct {
	Date string           `json:"date"`
	Apps map[string]int64 `json:"apps"`
}

var (
	user32                        = windows.NewLazySystemDLL("user32.dll")
	kernel32                      = windows.NewLazySystemDLL("kernel32.dll")
	procGetForegroundWindow       = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessId  = user32.NewProc("GetWindowThreadProcessId")
	procOpenProcess               = kernel32.NewProc("OpenProcess")
	procQueryFullProcessImageName = kernel32.NewProc("QueryFullProcessImageNameW")
)

const (
	PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
	MAX_PATH                          = 260
)

func NewTracker() (*TimeTracker, error) {
	dataPath := filepath.Join(filepath.Dir(os.Args[0]), "time_tracking.json")

	tracker := &TimeTracker{
		dataPath:      dataPath,
		dailyData:     make(map[string]map[string]int64),
		retentionDays: 7, // Will be updated from config
	}

	// Load existing data
	if err := tracker.loadData(); err != nil {
		log.Printf("Failed to load existing time data: %v", err)
	}

	return tracker, nil
}

func (t *TimeTracker) Start(ctx context.Context) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	log.Println("Time tracker started")

	for {
		select {
		case <-ctx.Done():
			t.saveCurrentSession()
			return nil
		case <-ticker.C:
			t.updateActiveWindow()
		}
	}
}

func (t *TimeTracker) Stop() {
	t.saveCurrentSession()
	t.saveData()
	log.Println("Time tracker stopped")
}

func (t *TimeTracker) updateActiveWindow() {
	appName, err := t.getActiveWindowProcess()
	if err != nil {
		log.Printf("Failed to get active window process: %v", err)
		return
	}

	t.mutex.Lock()
	defer t.mutex.Unlock()

	now := time.Now()

	// Save previous session if app changed
	if t.currentApp != "" && t.currentApp != appName {
		duration := now.Sub(t.startTime).Seconds()
		t.addTimeToApp(t.currentApp, int64(duration))
	}

	// Start new session
	t.currentApp = appName
	t.startTime = now
}

func (t *TimeTracker) getActiveWindowProcess() (string, error) {
	// Get foreground window
	hWnd, _, _ := procGetForegroundWindow.Call()
	if hWnd == 0 {
		return "", fmt.Errorf("no foreground window")
	}

	// Get process ID
	var processID uint32
	procGetWindowThreadProcessId.Call(hWnd, uintptr(unsafe.Pointer(&processID)))
	if processID == 0 {
		return "", fmt.Errorf("failed to get process ID")
	}

	// Open process
	hProcess, _, _ := procOpenProcess.Call(
		PROCESS_QUERY_LIMITED_INFORMATION,
		0,
		uintptr(processID),
	)
	if hProcess == 0 {
		return "", fmt.Errorf("failed to open process")
	}
	defer windows.CloseHandle(windows.Handle(hProcess))

	// Get process image name
	var size uint32 = MAX_PATH
	buf := make([]uint16, MAX_PATH)
	ret, _, _ := procQueryFullProcessImageName.Call(
		hProcess,
		0, // Win32 path format
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)

	if ret == 0 {
		return "", fmt.Errorf("failed to get process image name")
	}

	// Convert to string and extract filename
	imagePath := windows.UTF16ToString(buf)
	appName := filepath.Base(imagePath)

	return appName, nil
}

func (t *TimeTracker) addTimeToApp(appName string, seconds int64) {
	date := time.Now().Format("2006-01-02")

	if t.dailyData[date] == nil {
		t.dailyData[date] = make(map[string]int64)
	}

	t.dailyData[date][appName] += seconds
}

func (t *TimeTracker) saveCurrentSession() {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	if t.currentApp != "" {
		duration := time.Now().Sub(t.startTime).Seconds()
		t.addTimeToApp(t.currentApp, int64(duration))
		t.currentApp = ""
	}
}

func (t *TimeTracker) loadData() error {
	if _, err := os.Stat(t.dataPath); os.IsNotExist(err) {
		return nil // File doesn't exist, start fresh
	}

	data, err := os.ReadFile(t.dataPath)
	if err != nil {
		return err
	}

	var timeData []TimeData
	if err := json.Unmarshal(data, &timeData); err != nil {
		return err
	}

	t.dailyData = make(map[string]map[string]int64)
	for _, day := range timeData {
		t.dailyData[day.Date] = day.Apps
	}

	return nil
}

func (t *TimeTracker) saveData() error {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	// Clean old data
	t.cleanOldData()

	// Convert to array format
	var timeData []TimeData
	for date, apps := range t.dailyData {
		timeData = append(timeData, TimeData{
			Date: date,
			Apps: apps,
		})
	}

	data, err := json.MarshalIndent(timeData, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(t.dataPath, data, 0644)
}

func (t *TimeTracker) cleanOldData() {
	cutoffDate := time.Now().AddDate(0, 0, -t.retentionDays).Format("2006-01-02")

	for date := range t.dailyData {
		if date < cutoffDate {
			delete(t.dailyData, date)
		}
	}
}

func (t *TimeTracker) GetTodayReport() map[string]int64 {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	date := time.Now().Format("2006-01-02")
	return t.dailyData[date]
}

func (t *TimeTracker) GetWeekReport() map[string]int64 {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	weekData := make(map[string]int64)
	now := time.Now()

	// Get last 7 days
	for i := 0; i < 7; i++ {
		date := now.AddDate(0, 0, -i).Format("2006-01-02")
		if dayData, exists := t.dailyData[date]; exists {
			for app, seconds := range dayData {
				weekData[app] += seconds
			}
		}
	}

	return weekData
}

func (t *TimeTracker) SetRetentionDays(days int) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	t.retentionDays = days
}
