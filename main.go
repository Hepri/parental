//go:build windows

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/Hepri/parental/internal/service"
)

const (
	serviceName = "ParentalControlBot"
	serviceDesc = "Parental Control Telegram Bot Service"
)

func main() {
	var (
		install   = flag.Bool("install", false, "Install the service")
		uninstall = flag.Bool("uninstall", false, "Uninstall the service")
		debugFlag = flag.Bool("debug", false, "Run in debug mode (not as service)")
		testFlag  = flag.Bool("test", false, "Test configuration and exit")
	)
	flag.Parse()

	if *install {
		if err := installService(); err != nil {
			log.Fatalf("Failed to install service: %v", err)
		}
		fmt.Println("Service installed successfully")
		return
	}

	if *uninstall {
		if err := uninstallService(); err != nil {
			log.Fatalf("Failed to uninstall service: %v", err)
		}
		fmt.Println("Service uninstalled successfully")
		return
	}

	if *testFlag {
		if err := testConfiguration(); err != nil {
			log.Fatalf("Configuration test failed: %v", err)
		}
		fmt.Println("Configuration test passed!")
		return
	}

	// Check if running as service
	isInteractive := isInteractiveSession()

	if !isInteractive || *debugFlag {
		// Running as service or debug mode
		runService(*debugFlag)
	} else {
		// Running interactively
		fmt.Println("Parental Control Bot")
		fmt.Println("===================")
		fmt.Println("Available commands:")
		fmt.Println("  -install   : Install as Windows service")
		fmt.Println("  -uninstall : Remove Windows service")
		fmt.Println("  -debug     : Run in debug mode (not as service)")
		fmt.Println("  -test      : Test configuration and exit")
		fmt.Println()
		fmt.Println("For debugging, use: parental-control-bot.exe -debug")
	}
}

func runService(debug bool) {
	var err error
	if debug {
		// Run in debug mode - direct execution with console output
		fmt.Println("Starting Parental Control Bot in DEBUG mode...")
		fmt.Println("Press Ctrl+C to stop")

		// Create context for graceful shutdown
		ctx, cancel := context.WithCancel(context.Background())

		// Handle Ctrl+C
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

		go func() {
			sig := <-sigChan
			fmt.Printf("\nReceived signal: %v\n", sig)
			fmt.Println("Shutting down...")
			cancel()
		}()

		// Initialize and run service directly
		service := &service.ParentalControlService{}
		err = service.RunDebug(ctx)
		if err != nil {
			log.Fatalf("Debug mode failed: %v", err)
		}
	} else {
		// Run as Windows service
		err = svc.Run(serviceName, &service.ParentalControlService{})
	}
	if err != nil {
		log.Fatalf("Service failed: %v", err)
	}
}

// isInteractiveSession determines if the application is running in an interactive session
func isInteractiveSession() bool {
	// Check if stdout is connected to a console
	var mode uint32
	err := syscall.GetConsoleMode(syscall.Stdout, &mode)
	return err == nil
}

// testConfiguration tests the configuration without starting the service
func testConfiguration() error {
	fmt.Println("Testing configuration...")

	// Load configuration
	configPath := "config.json"
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return fmt.Errorf("config.json not found. Please copy config.json.example to config.json and configure it")
	}

	// Try to load config
	cfg, err := service.LoadConfigForTest(configPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %v", err)
	}

	fmt.Printf("✓ Configuration loaded successfully\n")
	fmt.Printf("✓ Telegram bot token configured\n")
	fmt.Printf("✓ Authorized users: %d\n", len(cfg.AuthorizedUserIDs))
	fmt.Printf("✓ Child accounts: %d\n", len(cfg.ChildAccounts))
	fmt.Printf("✓ Data retention: %d days\n", cfg.DataRetentionDays)

	// Test Telegram bot connection
	fmt.Println("Testing Telegram bot connection...")
	_, err = service.TestBotConnection(cfg)
	if err != nil {
		return fmt.Errorf("Telegram bot connection failed: %v", err)
	}

	fmt.Printf("✓ Telegram bot connected successfully\n")

	return nil
}

func installService() error {
	exepath, err := os.Executable()
	if err != nil {
		return err
	}

	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err == nil {
		s.Close()
		return fmt.Errorf("service %s already exists", serviceName)
	}

	s, err = m.CreateService(serviceName, exepath, mgr.Config{
		DisplayName:      serviceDesc,
		Description:      serviceDesc,
		StartType:        mgr.StartAutomatic,
		ServiceStartName: "LocalSystem",
	}, "is", "auto-started")
	if err != nil {
		return err
	}
	defer s.Close()

	// Configure service recovery options
	err = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 0},
		{Type: mgr.ServiceRestart, Delay: 0},
		{Type: mgr.ServiceRestart, Delay: 0},
	}, 0)
	if err != nil {
		return err
	}

	// Set service description
	// Note: SetDescription might not be available in all versions
	// err = s.SetDescription(serviceDesc)
	// if err != nil {
	//     return err
	// }

	// Start the service
	err = s.Start()
	if err != nil {
		return err
	}

	return nil
}

func uninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %s is not installed", serviceName)
	}
	defer s.Close()

	err = s.Delete()
	if err != nil {
		return err
	}

	return nil
}
