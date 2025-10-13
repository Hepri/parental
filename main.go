//go:build windows

package main

import (
	"flag"
	"fmt"
	"log"
	"os"

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

	// Check if running as service
	isIntSess, err := svc.IsAnInteractiveSession()
	if err != nil {
		log.Fatalf("Failed to determine if running in interactive session: %v", err)
	}

	if !isIntSess || *debugFlag {
		// Running as service or debug mode
		runService(*debugFlag)
	} else {
		// Running interactively
		fmt.Println("Use -install to install the service or -debug to run in debug mode")
		fmt.Println("Use -uninstall to remove the service")
	}
}

func runService(debug bool) {
	var err error
	if debug {
		// For debug mode, we'll just run the service logic directly
		// In a real implementation, you would use debug.Run here
		fmt.Println("Debug mode not implemented in this build")
		return
	} else {
		err = svc.Run(serviceName, &service.ParentalControlService{})
	}
	if err != nil {
		log.Fatalf("Service failed: %v", err)
	}
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
