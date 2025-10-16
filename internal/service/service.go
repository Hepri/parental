//go:build windows

package service

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"

	"github.com/Hepri/parental/internal/bot"
	"github.com/Hepri/parental/internal/config"
	"github.com/Hepri/parental/internal/session"
	"github.com/Hepri/parental/internal/shutdown"
	"github.com/Hepri/parental/internal/tracker"
)

type ParentalControlService struct {
	config      *config.Config
	bot         *bot.TelegramBot
	tracker     *tracker.TimeTracker
	sessionMgr  *session.Manager
	shutdownMgr *shutdown.ShutdownManager
	ctx         context.Context
	cancel      context.CancelFunc
}

func (s *ParentalControlService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPauseAndContinue
	changes <- svc.Status{State: svc.StartPending}

	// Initialize service
	if err := s.initialize(); err != nil {
		log.Printf("Failed to initialize service: %v", err)
		changes <- svc.Status{State: svc.Stopped}
		return false, 1
	}

	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

	// Start background goroutines
	go s.runBot()
	go s.runTimeTracker()
	go s.runSessionMonitor()

	// Handle service control requests
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				log.Println("Service stopping...")
				s.cleanup()
				changes <- svc.Status{State: svc.StopPending}
				return false, 0
			case svc.Pause:
				changes <- svc.Status{State: svc.Paused, Accepts: cmdsAccepted}
			case svc.Continue:
				changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}
			default:
				log.Printf("Unexpected control request #%d", c)
			}
		case <-s.ctx.Done():
			log.Println("Service context cancelled")
			s.cleanup()
			changes <- svc.Status{State: svc.StopPending}
			return false, 0
		}
	}
}

func (s *ParentalControlService) initialize() error {
	// Create context for graceful shutdown
	s.ctx, s.cancel = context.WithCancel(context.Background())

	// Load configuration
	configPath := filepath.Join(filepath.Dir(os.Args[0]), "config.json")
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %v", err)
	}
	s.config = cfg

	// Ensure child accounts exist
	if err := config.EnsureChildAccounts(s.config); err != nil {
		return fmt.Errorf("failed to ensure child accounts: %v", err)
	}

	// Initialize session manager
	s.sessionMgr, err = session.NewManager(s.config.ChildAccounts)
	if err != nil {
		return fmt.Errorf("failed to initialize session manager: %v", err)
	}

	// Initialize time tracker
	s.tracker, err = tracker.NewTracker()
	if err != nil {
		return fmt.Errorf("failed to initialize time tracker: %v", err)
	}

	// Initialize shutdown manager
	s.shutdownMgr = shutdown.NewShutdownManager()

	// Initialize Telegram bot
	s.bot, err = bot.NewBot(s.config, s.sessionMgr, s.tracker, s.shutdownMgr)
	if err != nil {
		return fmt.Errorf("failed to initialize Telegram bot: %v", err)
	}

	// Setup event logging
	elog, err := eventlog.Open("ParentalControlBot")
	if err != nil {
		log.Printf("Failed to open event log: %v", err)
	} else {
		elog.Info(1, "Parental Control Bot Service started")
		elog.Close()
	}

	log.Println("Service initialized successfully")
	return nil
}

func (s *ParentalControlService) runBot() {
	log.Println("Starting Telegram bot...")
	if err := s.bot.Start(s.ctx); err != nil {
		log.Printf("Bot stopped with error: %v", err)
		// Не вызываем s.cancel() здесь, так как бот теперь сам управляет переподключением
		// Сервис будет продолжать работать, а бот будет пытаться переподключиться
	}
}

func (s *ParentalControlService) runTimeTracker() {
	log.Println("Starting time tracker...")
	if err := s.tracker.Start(s.ctx); err != nil {
		log.Printf("Time tracker error: %v", err)
		s.cancel() // Signal service to stop
	}
}

func (s *ParentalControlService) runSessionMonitor() {
	log.Println("Starting session monitor...")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			// Check for expired sessions and lock them
			expiredSessions := s.sessionMgr.GetExpiredSessions()
			for _, session := range expiredSessions {
				log.Printf("Session expired for user: %s", session.Username)
				if err := s.sessionMgr.LockSession(session.Username); err != nil {
					log.Printf("Failed to lock expired session for %s: %v", session.Username, err)
				} else {
					// Notify bot about expired session
					s.bot.NotifySessionExpired(session.Username)
				}
			}
		}
	}
}

func (s *ParentalControlService) cleanup() {
	log.Println("Cleaning up service...")

	if s.cancel != nil {
		s.cancel()
	}

	if s.bot != nil {
		s.bot.Stop()
	}

	if s.tracker != nil {
		s.tracker.Stop()
	}

	if s.sessionMgr != nil {
		s.sessionMgr.Cleanup()
	}

	// Log service stop
	elog, err := eventlog.Open("ParentalControlBot")
	if err == nil {
		elog.Info(1, "Parental Control Bot Service stopped")
		elog.Close()
	}

	log.Println("Service cleanup completed")
}

// RunDebug runs the service in debug mode (not as Windows service)
func (s *ParentalControlService) RunDebug(ctx context.Context) error {
	fmt.Println("Initializing Parental Control Bot in debug mode...")

	// Initialize service
	if err := s.initialize(); err != nil {
		return fmt.Errorf("failed to initialize service: %v", err)
	}

	fmt.Println("✓ Service initialized successfully")
	fmt.Println("✓ Telegram bot started")
	fmt.Println("✓ Time tracker started")
	fmt.Println("✓ Session monitor started")
	fmt.Println()
	fmt.Println("Bot is running! You can now test it via Telegram.")
	fmt.Println("Press Ctrl+C to stop...")

	// Start background goroutines
	go s.runBot()
	go s.runTimeTracker()
	go s.runSessionMonitor()

	// Wait for context cancellation
	<-ctx.Done()

	fmt.Println("Shutting down debug mode...")
	s.cleanup()
	fmt.Println("Debug mode stopped")

	return nil
}

// LoadConfigForTest loads configuration for testing purposes
func LoadConfigForTest(configPath string) (*config.Config, error) {
	return config.LoadConfig(configPath)
}

// TestBotConnection tests the Telegram bot connection
func TestBotConnection(cfg *config.Config) (*bot.TelegramBot, error) {
	// Create a minimal bot instance for testing
	bot, err := bot.NewBot(cfg, nil, nil, nil)
	if err != nil {
		return nil, err
	}

	// Test the connection by getting bot info
	_, err = bot.GetMe()
	if err != nil {
		return nil, err
	}

	return bot, nil
}
