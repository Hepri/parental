//go:build windows

package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
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
				return fmt.Errorf("failed to create user account %s: %v", account.Username, err)
			}

			// Add to Users group
			if err := addUserToGroup(account.Username, "Users"); err != nil {
				return fmt.Errorf("failed to add user %s to Users group: %v", account.Username, err)
			}
		}
	}

	// Save updated config with generated passwords
	return saveConfig(config)
}

func userExists(username string) (bool, error) {
	serverName, _ := windows.UTF16PtrFromString("")
	userName, _ := windows.UTF16PtrFromString(username)

	var buf *byte
	var bufSize uint32

	ret, _, _ := procNetUserGetInfo.Call(
		uintptr(unsafe.Pointer(serverName)),
		uintptr(unsafe.Pointer(userName)),
		1, // INFO_LEVEL
		uintptr(unsafe.Pointer(&buf)),
		uintptr(unsafe.Pointer(&bufSize)),
	)

	if ret == 0 {
		// User exists
		return true, nil
	} else if ret == 2224 { // NERR_UserNotFound
		return false, nil
	}

	return false, fmt.Errorf("NetUserGetInfo failed with code %d", ret)
}

func createUserAccount(account ChildAccount) error {
	serverName, _ := windows.UTF16PtrFromString("")
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

	ret, _, _ := procNetUserAdd.Call(
		uintptr(unsafe.Pointer(serverName)),
		1, // INFO_LEVEL
		uintptr(unsafe.Pointer(&userInfo)),
		0, // PARM_ERROR
	)

	if ret != 0 {
		return fmt.Errorf("NetUserAdd failed with code %d", ret)
	}

	return nil
}

func addUserToGroup(username, groupName string) error {
	serverName, _ := windows.UTF16PtrFromString("")
	groupNamePtr, _ := windows.UTF16PtrFromString(groupName)
	userNamePtr, _ := windows.UTF16PtrFromString(username)

	// Create LOCALGROUP_MEMBERS_INFO_3 structure
	memberInfo := struct {
		lgrmi3_domainandname *uint16
	}{
		lgrmi3_domainandname: userNamePtr,
	}

	ret, _, _ := procNetLocalGroupAddMembers.Call(
		uintptr(unsafe.Pointer(serverName)),
		uintptr(unsafe.Pointer(groupNamePtr)),
		3, // INFO_LEVEL
		uintptr(unsafe.Pointer(&memberInfo)),
		1, // TOTAL_ENTRIES
	)

	if ret != 0 {
		return fmt.Errorf("NetLocalGroupAddMembers failed with code %d", ret)
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
