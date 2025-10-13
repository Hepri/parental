# Windows Parental Control Telegram Bot

A Windows service that provides parental control functionality through a Telegram bot interface. This service allows parents to remotely manage their child's computer access, track application usage time, and control computer shutdown.

## Features

- **🟢 Grant Access**: Start timed sessions for child users with automatic lockout
- **🔒 Lock Sessions**: Immediately lock child user sessions
- **📊 Time Tracking**: Monitor active application usage with detailed reports
- **⚙️ Computer Control**: Schedule or immediately shutdown the computer
- **🛡️ Security**: Admin-only configuration, protected service, automatic account creation
- **📱 Button Interface**: User-friendly Telegram bot with inline keyboards

## Prerequisites

- Windows 10/11 (64-bit)
- Go 1.24.5 or later
- Administrator privileges for installation
- Telegram Bot Token (from [@BotFather](https://t.me/BotFather))

## Installation

### 1. Build the Service

```bash
# Clone or download the project
cd /path/to/parental-control-bot

# Install dependencies
go mod tidy

# Build the executable
go build -o parental-control-bot.exe
```

### 2. Configure the Bot

1. Copy `config.json.example` to `config.json`
2. Edit `config.json` with your settings:

```json
{
  "telegram_bot_token": "YOUR_BOT_TOKEN_HERE",
  "authorized_user_ids": [123456789],
  "child_accounts": [
    {
      "username": "child1",
      "full_name": "Child One",
      "password": "auto-generated-on-creation"
    }
  ],
  "data_retention_days": 7
}
```

**Configuration Details:**
- `telegram_bot_token`: Get this from [@BotFather](https://t.me/BotFather)
- `authorized_user_ids`: Your Telegram user ID (use [@userinfobot](https://t.me/userinfobot) to get it)
- `child_accounts`: List of child user accounts to manage
- `data_retention_days`: How long to keep time tracking data

### 3. Install as Windows Service

**Run as Administrator:**

```cmd
# Install the service
parental-control-bot.exe -install

# The service will start automatically
```

### 4. Verify Installation

1. Open Services (`services.msc`)
2. Find "Parental Control Bot Service"
3. Verify it's running and set to "Automatic" startup

## Usage

### Starting the Bot

1. Open Telegram and find your bot
2. Send `/start` to begin
3. Use the button interface to navigate

### Main Features

#### 🟢 Grant Access
- Select child account
- Choose duration (15min, 30min, 1hr, 2hr, or custom)
- Session starts automatically
- Child can log in and use computer
- Session locks automatically when time expires

#### 🔒 Lock Session
- View all active sessions
- Lock individual sessions or all at once
- Immediate effect

#### 📊 View Statistics
- **Today's Report**: See what applications were used today
- **This Week's Report**: Weekly usage summary
- Time tracked in minutes per application

#### ⚙️ Computer Control
- **Status**: View active sessions and scheduled shutdowns
- **Shutdown Now**: Immediate shutdown (30 seconds)
- **Schedule Shutdown**: Delay shutdown (5min, 15min, 30min, 1hr)
- **Cancel Shutdown**: Cancel scheduled shutdown

## Security Features

### Service Protection
- Runs as SYSTEM with highest privileges
- Auto-starts on boot
- Restarts automatically on failure
- Requires administrator privileges to stop/modify
- Protected configuration file (admin-only access)

### Account Management
- Automatically creates child accounts if they don't exist
- Generates strong random passwords
- Accounts are standard users (not administrators)
- Passwords never expire and cannot be changed by users
- Accounts cannot be deleted by non-admin users

### Access Control
- Only whitelisted Telegram users can control the bot
- All unauthorized access attempts are logged
- Configuration file is protected with Windows ACLs

## File Structure

```
parental-control-bot/
├── main.go                    # Service entry point
├── config.json.example        # Configuration template
├── config.json               # Your configuration (created)
├── time_tracking.json        # Time tracking data (created)
├── internal/
│   ├── bot/                  # Telegram bot implementation
│   ├── config/               # Configuration management
│   ├── service/              # Windows service wrapper
│   ├── session/              # Session management
│   ├── shutdown/             # Shutdown control
│   └── tracker/              # Time tracking
└── README.md                 # This file
```

## Troubleshooting

### Service Won't Start
1. Check Windows Event Log for errors
2. Verify `config.json` exists and is valid
3. Ensure bot token is correct
4. Run as administrator

### Bot Not Responding
1. Verify Telegram bot token is correct
2. Check if your user ID is in `authorized_user_ids`
3. Ensure internet connection is working
4. Check Windows Event Log for bot errors

### Child Account Issues
1. Service automatically creates accounts on startup
2. Check Windows Event Log for account creation errors
3. Verify account names don't conflict with existing users
4. Ensure service has sufficient privileges

### Time Tracking Not Working
1. Check if `time_tracking.json` is being created
2. Verify service has permission to write files
3. Check Windows Event Log for tracking errors

## Uninstallation

**Run as Administrator:**

```cmd
# Stop the service
net stop "Parental Control Bot Service"

# Uninstall the service
parental-control-bot.exe -uninstall

# Delete files
del parental-control-bot.exe
del config.json
del time_tracking.json
```

## Development

### Building from Source

```bash
# Install dependencies
go mod tidy

# Build for Windows
GOOS=windows GOARCH=amd64 go build -o parental-control-bot.exe

# Run in debug mode
parental-control-bot.exe -debug
```

### Testing

```bash
# Run tests
go test ./...

# Run with coverage
go test -cover ./...
```

## License

This project is provided as-is for educational and personal use. Please ensure compliance with local laws and regulations regarding parental control software.

## Support

For issues or questions:
1. Check the troubleshooting section above
2. Review Windows Event Logs
3. Verify configuration settings
4. Ensure all prerequisites are met

## Security Notice

This software requires SYSTEM-level privileges to function properly. Only install on trusted systems and ensure proper access controls are in place. The service is designed to be tamper-resistant but should not be considered a replacement for proper system security measures.
