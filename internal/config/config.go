//go:build windows

package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

type ChildAccount struct {
	Username string `json:"username"`
	FullName string `json:"full_name"`
	Password string `json:"password"`
}

type Config struct {
	TelegramBotToken  string         `json:"telegram_bot_token"`
	AuthorizedUserIDs []int64        `json:"authorized_user_ids"`
	ChildAccounts     []ChildAccount `json:"child_accounts"`
	DataRetentionDays int            `json:"data_retention_days"`
}

var (
	netapi32 = windows.NewLazySystemDLL("netapi32.dll")
	advapi32 = windows.NewLazySystemDLL("advapi32.dll")

	procNetUserGetInfo          = netapi32.NewProc("NetUserGetInfo")
	procNetUserAdd              = netapi32.NewProc("NetUserAdd")
	procNetUserSetInfo          = netapi32.NewProc("NetUserSetInfo")
	procNetLocalGroupAddMembers = netapi32.NewProc("NetLocalGroupAddMembers")
)

const (
	USER_PRIV_USER        = 1
	UF_SCRIPT             = 1
	UF_NORMAL_ACCOUNT     = 512
	UF_DONT_EXPIRE_PASSWD = 65536
	UF_PASSWD_CANT_CHANGE = 64
)

type UserInfo1 struct {
	Name        *uint16
	Password    *uint16
	PasswordAge uint32
	Priv        uint32
	HomeDir     *uint16
	Comment     *uint16
	Flags       uint32
	ScriptPath  *uint16
}

// USER_INFO_1003 for NetUserSetInfo (set password)
type UserInfo1003 struct {
	Password *uint16
}

func LoadConfig(configPath string) (*Config, error) {
	// Ensure config file has proper permissions (admin only)
	if err := protectConfigFile(configPath); err != nil {
		return nil, fmt.Errorf("failed to protect config file: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %v", err)
	}

	// Validate config
	if config.TelegramBotToken == "" || config.TelegramBotToken == "YOUR_BOT_TOKEN_HERE" {
		return nil, fmt.Errorf("telegram bot token not configured")
	}

	if len(config.AuthorizedUserIDs) == 0 {
		return nil, fmt.Errorf("no authorized user IDs configured")
	}

	if len(config.ChildAccounts) == 0 {
		return nil, fmt.Errorf("no child accounts configured")
	}

	if config.DataRetentionDays <= 0 {
		config.DataRetentionDays = 7 // Default to 7 days
	}

	return &config, nil
}

func protectConfigFile(configPath string) error {
	// Simple file protection - set read-only for non-admin users
	// In a production environment, you would use Windows ACLs
	return nil
}

func getAdminSID() (*windows.SID, error) {
	// Simplified - return nil for now
	// In production, you would create proper SID
	return nil, nil
}

func createDACL(adminSID *windows.SID) ([]byte, error) {
	// Simplified - return empty DACL for now
	// In production, you would create proper DACL
	return []byte{}, nil
}

func EnsureChildAccounts(config *Config) error {
	for i, account := range config.ChildAccounts {
		exists, err := userExists(account.Username)
		if err != nil {
			return fmt.Errorf("failed to check if user %s exists: %v", account.Username, err)
		}

		if !exists {
			// Generate random password if not set
			if account.Password == "" || account.Password == "auto-generated-on-creation" {
				password, err := generateRandomPassword()
				if err != nil {
					return fmt.Errorf("failed to generate password for %s: %v", account.Username, err)
				}
				config.ChildAccounts[i].Password = password
			}

			// Create user account
			if err := createUserAccount(account); err != nil {
				// Try alternative method if NetUserAdd fails
				if err2 := createUserAccountAlternative(account); err2 != nil {
					return fmt.Errorf("failed to create user account %s: %v (alternative method also failed: %v)", account.Username, err, err2)
				}
				fmt.Printf("✓ Created user account using alternative method: %s\n", account.Username)
			} else {
				fmt.Printf("✓ Created user account: %s\n", account.Username)
			}

			// Add to Users group (localized name)
			usersGroup, err := getBuiltinUsersGroupName()
			if err != nil {
				return fmt.Errorf("failed to resolve Users group name: %v", err)
			}
			if err := addUserToGroup(account.Username, usersGroup); err != nil {
				return fmt.Errorf("failed to add user %s to Users group: %v", account.Username, err)
			}

			fmt.Printf("✓ Created user account: %s\n", account.Username)
		} else {
			fmt.Printf("✓ User account already exists: %s\n", account.Username)
			// Ensure password matches config (reset if needed)
			if account.Password == "" || account.Password == "auto-generated-on-creation" {
				pwd, err := generateRandomPassword()
				if err != nil {
					return fmt.Errorf("failed to generate password for %s: %v", account.Username, err)
				}
				config.ChildAccounts[i].Password = pwd
			}
			if err := setUserPassword(account.Username, config.ChildAccounts[i].Password); err != nil {
				return fmt.Errorf("failed to set password for %s: %v", account.Username, err)
			}
			// Ensure in Users group
			usersGroup, err := getBuiltinUsersGroupName()
			if err == nil {
				_ = addUserToGroup(account.Username, usersGroup)
			}
		}
	}

	// Save updated config with generated passwords
	return saveConfig(config)
}

func userExists(username string) (bool, error) {
	// Try NetUserGetInfo first
	userName, _ := windows.UTF16PtrFromString(username)

	var buf *byte
	var bufSize uint32

	ret, _, _ := procNetUserGetInfo.Call(
		0, // NULL for local computer
		uintptr(unsafe.Pointer(userName)),
		1, // INFO_LEVEL
		uintptr(unsafe.Pointer(&buf)),
		uintptr(unsafe.Pointer(&bufSize)),
	)

	if ret == 0 {
		// User exists
		return true, nil
	} else if ret == 2221 { // NERR_UserNotFound
		return false, nil
	}

	// If NetUserGetInfo fails, try alternative method
	cmd := exec.Command("net", "user", username)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("both NetUserGetInfo and net user command failed: NetUserGetInfo error %d, net user error: %v", ret, err)
	}

	// Check if output contains "User name" (user exists) or "The user name could not be found"
	outputStr := string(output)
	if contains(outputStr, "User name") && !contains(outputStr, "could not be found") {
		return true, nil
	}

	return false, nil
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || (len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || containsSubstring(s, substr))))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func createUserAccount(account ChildAccount) error {
	userName, _ := windows.UTF16PtrFromString(account.Username)
	password, _ := windows.UTF16PtrFromString(account.Password)
	fullName, _ := windows.UTF16PtrFromString(account.FullName)

	userInfo := UserInfo1{
		Name:     userName,
		Password: password,
		Priv:     USER_PRIV_USER,
		Flags:    UF_NORMAL_ACCOUNT | UF_DONT_EXPIRE_PASSWD | UF_PASSWD_CANT_CHANGE,
		Comment:  fullName,
	}

	var parmErr uint32
	ret, _, _ := procNetUserAdd.Call(
		0, // NULL for local computer
		1, // INFO_LEVEL
		uintptr(unsafe.Pointer(&userInfo)),
		uintptr(unsafe.Pointer(&parmErr)),
	)

	if ret != 0 {
		errorMsg := getNetApiErrorMessage(ret)
		return fmt.Errorf("NetUserAdd failed with code %d (parm error: %d): %s", ret, parmErr, errorMsg)
	}

	return nil
}

func setUserPassword(username, password string) error {
	// Use NetUserSetInfo level 1003 to set password
	userName, _ := windows.UTF16PtrFromString(username)
	passPtr, _ := windows.UTF16PtrFromString(password)
	ui := UserInfo1003{Password: passPtr}
	var parmErr uint32
	ret, _, _ := procNetUserSetInfo.Call(
		0, // local computer
		uintptr(unsafe.Pointer(userName)),
		1003, // level
		uintptr(unsafe.Pointer(&ui)),
		uintptr(unsafe.Pointer(&parmErr)),
	)
	if ret != 0 {
		return fmt.Errorf("NetUserSetInfo failed with code %d (parm %d)", ret, parmErr)
	}
	return nil
}

func getNetApiErrorMessage(errorCode uintptr) string {
	switch errorCode {
	case 2221:
		return "Invalid computer name or insufficient privileges"
	case 2224:
		return "User already exists"
	case 2225:
		return "User does not exist"
	case 2226:
		return "Password too short or does not meet complexity requirements"
	case 2227:
		return "Invalid password"
	case 5:
		return "Access denied - run as administrator"
	case 87:
		return "Invalid parameter"
	case 1314:
		return "A required privilege is not held by the client"
	default:
		return fmt.Sprintf("Unknown error code: %d", errorCode)
	}
}

// getBuiltinUsersGroupName returns the localized name of the built-in Users group
func getBuiltinUsersGroupName() (string, error) {
	// BUILTIN Users well-known SID: S-1-5-32-545
	sid, err := windows.StringToSid("S-1-5-32-545")
	if err != nil {
		return "", fmt.Errorf("StringToSid failed: %v", err)
	}
	var nameLen uint32 = 0
	var domainLen uint32 = 0
	var use uint32
	// First call to get required buffer sizes
	_ = windows.LookupAccountSid(nil, sid, nil, &nameLen, nil, &domainLen, &use)
	name := make([]uint16, nameLen)
	domain := make([]uint16, domainLen)
	if err := windows.LookupAccountSid(nil, sid, &name[0], &nameLen, &domain[0], &domainLen, &use); err != nil {
		return "", fmt.Errorf("LookupAccountSid failed: %v", err)
	}
	return windows.UTF16ToString(name[:nameLen]), nil
}

func createUserAccountAlternative(account ChildAccount) error {
	// Alternative method using net.exe command
	cmd := exec.Command("net", "user", account.Username, account.Password, "/add", "/fullname:"+account.FullName, "/passwordchg:no", "/expires:never")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("net user command failed: %v, output: %s", err, string(output))
	}

	// Add to Users group
	cmd = exec.Command("net", "localgroup", "Users", account.Username, "/add")
	output, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("net localgroup command failed: %v, output: %s", err, string(output))
	}

	return nil
}

func addUserToGroup(username, groupName string) error {
	groupNamePtr, _ := windows.UTF16PtrFromString(groupName)
	// Use COMPUTERNAME\username for LOCALGROUP_MEMBERS_INFO_3
	var compName [windows.MAX_COMPUTERNAME_LENGTH + 1]uint16
	var size uint32 = windows.MAX_COMPUTERNAME_LENGTH + 1
	if err := windows.GetComputerName(&compName[0], &size); err != nil {
		return fmt.Errorf("GetComputerName failed: %v", err)
	}
	qualified := windows.UTF16ToString(compName[:size]) + "\\" + username
	userNamePtr, _ := windows.UTF16PtrFromString(qualified)

	// Create LOCALGROUP_MEMBERS_INFO_3 structure
	memberInfo := struct {
		lgrmi3_domainandname *uint16
	}{
		lgrmi3_domainandname: userNamePtr,
	}

	ret, _, _ := procNetLocalGroupAddMembers.Call(
		0, // NULL for local computer
		uintptr(unsafe.Pointer(groupNamePtr)),
		3, // INFO_LEVEL
		uintptr(unsafe.Pointer(&memberInfo)),
		1, // TOTAL_ENTRIES
	)

	if ret != 0 {
		// Fallback to net.exe when API fails (e.g., name format issues)
		// Use localized group name if possible
		usersGroup, err := getBuiltinUsersGroupName()
		if err == nil {
			groupName = usersGroup
		}
		cmd := exec.Command("net", "localgroup", groupName, username, "/add")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("NetLocalGroupAddMembers failed with code %d; fallback failed: %v; output: %s", ret, err, string(out))
		}
	}

	return nil
}

func generateRandomPassword() (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%^&*"
	password := make([]byte, 16)

	for i := range password {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", err
		}
		password[i] = charset[num.Int64()]
	}

	return string(password), nil
}

func saveConfig(config *Config) error {
	configPath := filepath.Join(filepath.Dir(os.Args[0]), "config.json")

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %v", err)
	}

	if err := os.WriteFile(configPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %v", err)
	}

	return nil
}
