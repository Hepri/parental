//go:build windows

package session

import (
	"fmt"
	"log"
	"sync"
	"time"
	"unsafe"

	"github.com/Hepri/parental/internal/config"
	"golang.org/x/sys/windows"
)

type ActiveSession struct {
	Username  string
	StartTime time.Time
	Duration  time.Duration
	IsActive  bool
}

type Manager struct {
	childAccounts  []config.ChildAccount
	activeSessions map[string]*ActiveSession
	mutex          sync.RWMutex
}

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	wtsapi32 = windows.NewLazySystemDLL("wtsapi32.dll")
	advapi32 = windows.NewLazySystemDLL("advapi32.dll")
	userenv  = windows.NewLazySystemDLL("userenv.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procLockWorkStation              = user32.NewProc("LockWorkStation")
	procWTSEnumerateSessions         = wtsapi32.NewProc("WTSEnumerateSessionsW")
	procWTSQuerySessionInformation   = wtsapi32.NewProc("WTSQuerySessionInformationW")
	procLogonUser                    = advapi32.NewProc("LogonUserW")
	procCreateProcessAsUser          = advapi32.NewProc("CreateProcessAsUserW")
	procCreateProcessWithLogon       = advapi32.NewProc("CreateProcessWithLogonW")
	procWTSFreeMemory                = wtsapi32.NewProc("WTSFreeMemory")
	procWTSLogonUser                 = wtsapi32.NewProc("WTSLogonUserW")
	procDuplicateTokenEx             = advapi32.NewProc("DuplicateTokenEx")
	procSetTokenInformation          = advapi32.NewProc("SetTokenInformation")
	procCreateEnvironmentBlock       = userenv.NewProc("CreateEnvironmentBlock")
	procDestroyEnvironmentBlock      = userenv.NewProc("DestroyEnvironmentBlock")
	procLoadUserProfile              = userenv.NewProc("LoadUserProfileW")
	procUnloadUserProfile            = userenv.NewProc("UnloadUserProfileW")
	procWTSGetActiveConsoleSessionId = kernel32.NewProc("WTSGetActiveConsoleSessionId")
)

const (
	WTS_CURRENT_SERVER_HANDLE  = 0
	WTSActive                  = 0
	WTSDisconnected            = 1
	WTSConnected               = 2
	WTSConnectState            = 8
	WTSUserName                = 5
	WTSDomainName              = 7
	LOGON32_LOGON_INTERACTIVE  = 2
	LOGON32_PROVIDER_DEFAULT   = 0
	CREATE_UNICODE_ENVIRONMENT = 0x00000400
	TokenSessionId             = 12
)

type profileInfo struct {
	Size        uint32
	Flags       uint32
	UserName    *uint16
	ProfilePath *uint16
	DefaultPath *uint16
	ServerName  *uint16
	PolicyPath  *uint16
	ProfileGuid *uint16
	hProfile    windows.Handle
}

type WTS_SESSION_INFO struct {
	SessionID      uint32
	WinStationName *uint16
	State          uint32
}

func NewManager(childAccounts []config.ChildAccount) (*Manager, error) {
	return &Manager{
		childAccounts:  childAccounts,
		activeSessions: make(map[string]*ActiveSession),
	}, nil
}

func (m *Manager) GrantAccess(username string, duration time.Duration) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Find child account
	var account *config.ChildAccount
	for _, acc := range m.childAccounts {
		if acc.Username == username {
			account = &acc
			break
		}
	}

	if account == nil {
		return fmt.Errorf("child account %s not found", username)
	}

	// Check if user is already logged in
	sessions, err := m.getActiveSessions()
	if err != nil {
		return fmt.Errorf("failed to get active sessions: %v", err)
	}

	var targetSession *WTS_SESSION_INFO
	for _, session := range sessions {
		if session.State == WTSActive {
			// Get username for this session
			sessionUser, err := m.getSessionUsername(session.SessionID)
			if err != nil {
				continue
			}
			if sessionUser == username {
				targetSession = &session
				break
			}
		}
	}

	if targetSession == nil {
		// User is not logged in, need to log them in
		if err := m.logInUser(account); err != nil {
			return fmt.Errorf("failed to log in user %s: %v", username, err)
		}

		// Poll for session establishment (up to 30s)
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			sessions, err = m.getActiveSessions()
			if err != nil {
				return fmt.Errorf("failed to get updated sessions: %v", err)
			}
			for _, session := range sessions {
				if session.State == WTSActive || session.State == WTSConnected {
					sessionUser, err := m.getSessionUsername(session.SessionID)
					if err != nil {
						continue
					}
					if sessionUser == username {
						targetSession = &session
						break
					}
				}
			}
			if targetSession != nil {
				break
			}
			time.Sleep(1 * time.Second)
		}

		if targetSession == nil {
			return fmt.Errorf("failed to establish session for user %s", username)
		}
	}

	// Create active session record
	m.activeSessions[username] = &ActiveSession{
		Username:  username,
		StartTime: time.Now(),
		Duration:  duration,
		IsActive:  true,
	}

	log.Printf("Granted access to user %s for %v", username, duration)
	return nil
}

func (m *Manager) LockSession(username string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Remove from active sessions
	if session, exists := m.activeSessions[username]; exists {
		session.IsActive = false
		delete(m.activeSessions, username)
	}

	// Get active sessions to find the user's session
	sessions, err := m.getActiveSessions()
	if err != nil {
		return fmt.Errorf("failed to get active sessions: %v", err)
	}

	for _, session := range sessions {
		if session.State == WTSActive {
			sessionUser, err := m.getSessionUsername(session.SessionID)
			if err != nil {
				continue
			}
			if sessionUser == username {
				// Lock this session
				return m.lockSessionByID(session.SessionID)
			}
		}
	}

	return fmt.Errorf("user %s session not found", username)
}

func (m *Manager) LockAllSessions() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Clear all active sessions
	for username, session := range m.activeSessions {
		session.IsActive = false
		log.Printf("Locked session for user %s", username)
	}
	m.activeSessions = make(map[string]*ActiveSession)

	// Lock all child account sessions
	sessions, err := m.getActiveSessions()
	if err != nil {
		return fmt.Errorf("failed to get active sessions: %v", err)
	}

	for _, session := range sessions {
		if session.State == WTSActive {
			sessionUser, err := m.getSessionUsername(session.SessionID)
			if err != nil {
				continue
			}

			// Check if this is a child account
			for _, account := range m.childAccounts {
				if account.Username == sessionUser {
					if err := m.lockSessionByID(session.SessionID); err != nil {
						log.Printf("Failed to lock session for %s: %v", sessionUser, err)
					}
					break
				}
			}
		}
	}

	return nil
}

func (m *Manager) GetActiveSessions() map[string]*ActiveSession {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	result := make(map[string]*ActiveSession)
	for username, session := range m.activeSessions {
		if session.IsActive {
			result[username] = session
		}
	}

	return result
}

func (m *Manager) GetExpiredSessions() []*ActiveSession {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	var expired []*ActiveSession
	now := time.Now()

	for _, session := range m.activeSessions {
		if session.IsActive && now.Sub(session.StartTime) >= session.Duration {
			expired = append(expired, session)
		}
	}

	return expired
}

func (m *Manager) getActiveSessions() ([]WTS_SESSION_INFO, error) {
	var sessionInfo *WTS_SESSION_INFO
	var count uint32

	ret, _, _ := procWTSEnumerateSessions.Call(
		WTS_CURRENT_SERVER_HANDLE,
		0, // Reserved
		1, // Version
		uintptr(unsafe.Pointer(&sessionInfo)),
		uintptr(unsafe.Pointer(&count)),
	)

	if ret == 0 {
		return nil, fmt.Errorf("WTSEnumerateSessions failed")
	}

	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(sessionInfo)))

	sessions := make([]WTS_SESSION_INFO, count)
	for i := uint32(0); i < count; i++ {
		sessions[i] = *(*WTS_SESSION_INFO)(unsafe.Pointer(uintptr(unsafe.Pointer(sessionInfo)) + uintptr(i)*unsafe.Sizeof(*sessionInfo)))
	}

	return sessions, nil
}

func (m *Manager) getSessionUsername(sessionID uint32) (string, error) {
	var buffer *uint16
	var bytesReturned uint32

	ret, _, _ := procWTSQuerySessionInformation.Call(
		WTS_CURRENT_SERVER_HANDLE,
		uintptr(sessionID),
		WTSUserName,
		uintptr(unsafe.Pointer(&buffer)),
		uintptr(unsafe.Pointer(&bytesReturned)),
	)

	if ret == 0 {
		return "", fmt.Errorf("WTSQuerySessionInformation failed")
	}

	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(buffer)))

	return windows.UTF16PtrToString(buffer), nil
}

func (m *Manager) logInUser(account *config.ChildAccount) error {
	// Attempt to create a full Windows logon session similar to interactive login
	username, _ := windows.UTF16PtrFromString(account.Username)
	password, _ := windows.UTF16PtrFromString(account.Password)

	// Prefer userinit.exe to initialize user shell properly; fall back to explorer.exe where needed
	appNameUserInit, _ := windows.UTF16PtrFromString("C:\\Windows\\System32\\userinit.exe")
	appNameExplorer, _ := windows.UTF16PtrFromString("C:\\Windows\\explorer.exe")
	cmdLine := (*uint16)(nil)

	var startupInfo windows.StartupInfo
	startupInfo.Cb = uint32(unsafe.Sizeof(startupInfo))
	// Ensure interactive desktop
	desktop, _ := windows.UTF16PtrFromString("winsta0\\default")
	startupInfo.Desktop = desktop
	var processInfo windows.ProcessInformation

	// LOGON_WITH_PROFILE = 0x00000001
	// CREATE_NEW_CONSOLE  = 0x00000010
	const LOGON_WITH_PROFILE = 0x00000001
	const CREATE_NEW_CONSOLE = 0x00000010

	// First, try WTSLogonUser to create a real session, if available
	if procWTSLogonUser.Find() == nil {
		var userToken windows.Handle
		wtsr, _, _ := procWTSLogonUser.Call(
			WTS_CURRENT_SERVER_HANDLE,
			0, // Reserved
			uintptr(unsafe.Pointer(username)),
			uintptr(unsafe.Pointer(password)),
			2, // LOGON32_LOGON_INTERACTIVE
			0, // LOGON32_PROVIDER_DEFAULT
			uintptr(unsafe.Pointer(&userToken)),
		)
		if wtsr != 0 {
			// Start userinit in that session via CreateProcessAsUser
			var si windows.StartupInfo
			si.Cb = uint32(unsafe.Sizeof(si))
			desktop, _ := windows.UTF16PtrFromString("winsta0\\default")
			si.Desktop = desktop
			var pi windows.ProcessInformation
			app, _ := windows.UTF16PtrFromString("C:\\Windows\\System32\\userinit.exe")
			cr, _, _ := procCreateProcessAsUser.Call(
				uintptr(userToken),
				0,
				uintptr(unsafe.Pointer(app)),
				0, 0, 0,
				0,
				0,
				uintptr(unsafe.Pointer(&si)),
				uintptr(unsafe.Pointer(&pi)),
			)
			if cr != 0 {
				windows.CloseHandle(windows.Handle(pi.Process))
				windows.CloseHandle(windows.Handle(pi.Thread))
				return nil
			}
			// If CreateProcessAsUser failed, fall through to other methods
		}
	}

	// Try with NULL domain (local account) and proper environment/desktop
	// Build environment block for the user to avoid 0xC0000142 (DLL init failed)
	var env uintptr
	// CreateProcessWithLogonW ignores lpEnvironment unless CREATE_UNICODE_ENVIRONMENT is set
	// Let Windows build environment automatically by passing 0 for env here; if needed, we can switch to CreateEnvironmentBlock with CreateProcessAsUser path.
	ret, _, _ := procCreateProcessWithLogon.Call(
		uintptr(unsafe.Pointer(username)),
		0, // NULL domain
		uintptr(unsafe.Pointer(password)),
		LOGON_WITH_PROFILE,
		uintptr(unsafe.Pointer(appNameUserInit)),
		uintptr(unsafe.Pointer(cmdLine)),
		CREATE_NEW_CONSOLE,
		env, // Environment (0 = default for CreateProcessWithLogonW)
		0,   // Current directory
		uintptr(unsafe.Pointer(&startupInfo)),
		uintptr(unsafe.Pointer(&processInfo)),
	)
	if ret == 0 {
		// Try with explicit local domain "."
		domainDot, _ := windows.UTF16PtrFromString(".")
		ret, _, _ = procCreateProcessWithLogon.Call(
			uintptr(unsafe.Pointer(username)),
			uintptr(unsafe.Pointer(domainDot)),
			uintptr(unsafe.Pointer(password)),
			LOGON_WITH_PROFILE,
			uintptr(unsafe.Pointer(appNameUserInit)),
			uintptr(unsafe.Pointer(cmdLine)),
			CREATE_NEW_CONSOLE,
			0, 0,
			uintptr(unsafe.Pointer(&startupInfo)),
			uintptr(unsafe.Pointer(&processInfo)),
		)
		if ret == 0 {
			// Try explorer.exe with local domain as another fallback
			ret, _, _ = procCreateProcessWithLogon.Call(
				uintptr(unsafe.Pointer(username)),
				uintptr(unsafe.Pointer(domainDot)),
				uintptr(unsafe.Pointer(password)),
				LOGON_WITH_PROFILE,
				uintptr(unsafe.Pointer(appNameExplorer)),
				uintptr(unsafe.Pointer(cmdLine)),
				CREATE_NEW_CONSOLE,
				0, 0,
				uintptr(unsafe.Pointer(&startupInfo)),
				uintptr(unsafe.Pointer(&processInfo)),
			)
		}
		if ret == 0 {
			// Fallback: LogonUser + DuplicateTokenEx + SetTokenInformation(TokenSessionId) + CreateEnvironmentBlock + CreateProcessAsUser
			userPtr, _ := windows.UTF16PtrFromString(account.Username)
			passPtr, _ := windows.UTF16PtrFromString(account.Password)
			domPtr, _ := windows.UTF16PtrFromString(".")
			var token windows.Handle
			lr, _, lerr := procLogonUser.Call(
				uintptr(unsafe.Pointer(userPtr)),
				uintptr(unsafe.Pointer(domPtr)),
				uintptr(unsafe.Pointer(passPtr)),
				LOGON32_LOGON_INTERACTIVE,
				LOGON32_PROVIDER_DEFAULT,
				uintptr(unsafe.Pointer(&token)),
			)
			if lr == 0 {
				return fmt.Errorf("LogonUser failed: %v", lerr)
			}
			defer windows.CloseHandle(token)

			// Duplicate token with primary type
			var primaryToken windows.Handle
			const SecurityImpersonation = 2
			const TokenPrimary = 1
			dr, _, derr := procDuplicateTokenEx.Call(
				uintptr(token),
				windows.MAXIMUM_ALLOWED,
				0,
				SecurityImpersonation,
				TokenPrimary,
				uintptr(unsafe.Pointer(&primaryToken)),
			)
			if dr == 0 {
				return fmt.Errorf("DuplicateTokenEx failed: %v", derr)
			}
			defer windows.CloseHandle(primaryToken)

			// Load user profile (ensures registry hive and proper env variables)
			var pinfo profileInfo
			pinfo.Size = uint32(unsafe.Sizeof(pinfo))
			pinfo.UserName = userPtr
			_, _, _ = procLoadUserProfile.Call(
				uintptr(primaryToken),
				uintptr(unsafe.Pointer(&pinfo)),
			)

			// Assign console session ID to token
			sidR, _, _ := procWTSGetActiveConsoleSessionId.Call()
			sessionId := uint32(sidR)
			_, _, _ = procSetTokenInformation.Call(
				uintptr(primaryToken),
				TokenSessionId,
				uintptr(unsafe.Pointer(&sessionId)),
				unsafe.Sizeof(sessionId),
			)
			// Build environment for this token
			var envPtr uintptr
			_, _, _ = procCreateEnvironmentBlock.Call(
				uintptr(unsafe.Pointer(&envPtr)),
				uintptr(primaryToken),
				0,
			)
			// Create process as user with environment and desktop
			var si windows.StartupInfo
			si.Cb = uint32(unsafe.Sizeof(si))
			si.Desktop = desktop
			var pi windows.ProcessInformation
			cr, _, cerr := procCreateProcessAsUser.Call(
				uintptr(primaryToken),
				0,
				uintptr(unsafe.Pointer(appNameUserInit)),
				0, 0, 0,
				CREATE_UNICODE_ENVIRONMENT,
				envPtr,
				uintptr(unsafe.Pointer(&si)),
				uintptr(unsafe.Pointer(&pi)),
			)
			if cr == 0 {
				// Try explorer.exe as a last resort
				cr, _, cerr = procCreateProcessAsUser.Call(
					uintptr(primaryToken),
					0,
					uintptr(unsafe.Pointer(appNameExplorer)),
					0, 0, 0,
					CREATE_UNICODE_ENVIRONMENT,
					envPtr,
					uintptr(unsafe.Pointer(&si)),
					uintptr(unsafe.Pointer(&pi)),
				)
				if cr == 0 {
					return fmt.Errorf("CreateProcessAsUser failed: %v", cerr)
				}
			}
			windows.CloseHandle(windows.Handle(pi.Process))
			windows.CloseHandle(windows.Handle(pi.Thread))
			if envPtr != 0 {
				_, _, _ = procDestroyEnvironmentBlock.Call(envPtr)
			}
			if pinfo.hProfile != 0 {
				_, _, _ = procUnloadUserProfile.Call(
					uintptr(primaryToken),
					uintptr(pinfo.hProfile),
				)
			}
			return nil
		}
	}

	windows.CloseHandle(windows.Handle(processInfo.Process))
	windows.CloseHandle(windows.Handle(processInfo.Thread))
	return nil
}

func (m *Manager) lockSessionByID(sessionID uint32) error {
	// For now, we'll use LockWorkStation which locks the current session
	// In a more sophisticated implementation, you would target specific sessions
	ret, _, _ := procLockWorkStation.Call()
	if ret == 0 {
		return fmt.Errorf("LockWorkStation failed")
	}
	return nil
}

func (m *Manager) Cleanup() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Clear all active sessions
	for username, session := range m.activeSessions {
		session.IsActive = false
		log.Printf("Cleaned up session for user %s", username)
	}
	m.activeSessions = make(map[string]*ActiveSession)
}
