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

	log.Println("=== Parental Control Service Execute started ===")
	changes <- svc.Status{State: svc.StartPending}

	// Initialize service
	log.Println("Initializing service...")
	if err := s.initialize(); err != nil {
		log.Printf("Failed to initialize service: %v", err)
		changes <- svc.Status{State: svc.Stopped}
		return false, 1
	}

	log.Println("Service initialized successfully, changing status to Running")
	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

	// Start background goroutines
	log.Println("Starting background goroutines...")
	go s.runBot()
	go s.runTimeTracker()
	go s.runSessionMonitor()
	log.Println("All background goroutines started")

	// Handle service control requests
	log.Println("Entering main service loop...")
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				log.Println("Service interrogate request received")
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				log.Println("Service stop/shutdown request received")
				s.cleanup()
				changes <- svc.Status{State: svc.StopPending}
				log.Println("Service stopped")
				return false, 0
			case svc.Pause:
				log.Println("Service pause request received")
				changes <- svc.Status{State: svc.Paused, Accepts: cmdsAccepted}
			case svc.Continue:
				log.Println("Service continue request received")
				changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}
			default:
				log.Printf("Unexpected control request #%d", c)
			}
		case <-s.ctx.Done():
			log.Println("Service context cancelled")
			s.cleanup()
			changes <- svc.Status{State: svc.StopPending}
			log.Println("Service stopped after context cancellation")
			return false, 0
		}
	}
}

func (s *ParentalControlService) initialize() error {
	// Create context for graceful shutdown
	log.Println("Creating service context...")
	s.ctx, s.cancel = context.WithCancel(context.Background())

	// Load configuration
	configPath := filepath.Join(filepath.Dir(os.Args[0]), "config.json")
	log.Printf("Loading configuration from: %s", configPath)
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %v", err)
	}
	s.config = cfg
	log.Printf("Configuration loaded successfully. Authorized users: %d, Child accounts: %d",
		len(cfg.AuthorizedUserIDs), len(cfg.ChildAccounts))

	// Ensure child accounts exist
	log.Println("Ensuring child accounts exist...")
	if err := config.EnsureChildAccounts(s.config); err != nil {
		return fmt.Errorf("failed to ensure child accounts: %v", err)
	}
	log.Println("Child accounts verified/created successfully")

	// Initialize session manager
	log.Println("Initializing session manager...")
	s.sessionMgr, err = session.NewManager(s.config.ChildAccounts)
	if err != nil {
		return fmt.Errorf("failed to initialize session manager: %v", err)
	}
	log.Println("Session manager initialized")

	// Initialize time tracker
	log.Println("Initializing time tracker...")
	s.tracker, err = tracker.NewTracker()
	if err != nil {
		return fmt.Errorf("failed to initialize time tracker: %v", err)
	}
	log.Println("Time tracker initialized")

	// Initialize shutdown manager
	log.Println("Initializing shutdown manager...")
	s.shutdownMgr = shutdown.NewShutdownManager()
	log.Println("Shutdown manager initialized")

	// Initialize Telegram bot
	log.Println("Initializing Telegram bot...")
	s.bot, err = bot.NewBot(s.config, s.sessionMgr, s.tracker, s.shutdownMgr)
	if err != nil {
		return fmt.Errorf("failed to initialize Telegram bot: %v", err)
	}
	log.Println("Telegram bot initialized successfully")

	// Setup event logging
	elog, err := eventlog.Open("ParentalControlBot")
	if err != nil {
		log.Printf("Warning: Failed to open event log: %v", err)
	} else {
		elog.Info(1, "Parental Control Bot Service started")
		elog.Close()
		log.Println("Event log entry created")
	}

	log.Println("=== Service initialization completed successfully ===")
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
	log.Println("Session monitor started, checking every 30 seconds...")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	checkCount := 0
	for {
		select {
		case <-s.ctx.Done():
			log.Println("Session monitor stopped due to context cancellation")
			return
		case <-ticker.C:
			checkCount++
			log.Printf("Session monitor check #%d - Checking for expired sessions...", checkCount)

			// Check for expired sessions and lock them
			expiredSessions := s.sessionMgr.GetExpiredSessions()
			if len(expiredSessions) > 0 {
				log.Printf("Found %d expired session(s)", len(expiredSessions))
				for _, session := range expiredSessions {
					log.Printf("Session expired for user: %s", session.Username)
					if err := s.sessionMgr.LockSession(session.Username); err != nil {
						log.Printf("ERROR: Failed to lock expired session for %s: %v", session.Username, err)
					} else {
						log.Printf("Successfully locked session for user: %s", session.Username)
						// Notify bot about expired session
						s.bot.NotifySessionExpired(session.Username)
					}
				}
			} else {
				log.Printf("No expired sessions found")
			}
		}
	}
}

func (s *ParentalControlService) cleanup() {
	log.Println("=== Starting service cleanup ===")

	if s.cancel != nil {
		log.Println("Cancelling service context...")
		s.cancel()
	}

	if s.bot != nil {
		log.Println("Stopping Telegram bot...")
		s.bot.Stop()
		log.Println("Telegram bot stopped")
	}

	if s.tracker != nil {
		log.Println("Stopping time tracker...")
		s.tracker.Stop()
		log.Println("Time tracker stopped")
	}

	if s.sessionMgr != nil {
		log.Println("Cleaning up session manager...")
		s.sessionMgr.Cleanup()
		log.Println("Session manager cleaned up")
	}

	// Log service stop
	log.Println("Writing to event log...")
	elog, err := eventlog.Open("ParentalControlBot")
	if err == nil {
		elog.Info(1, "Parental Control Bot Service stopped")
		elog.Close()
		log.Println("Event log entry created")
	} else {
		log.Printf("Warning: Failed to write to event log: %v", err)
	}

	log.Println("=== Service cleanup completed ===")
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
