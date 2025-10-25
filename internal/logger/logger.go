//go:build windows

package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

var (
	logFile    *os.File
	defaultLog *log.Logger
)

// InitLogger инициализирует систему логирования с записью в файл
func InitLogger(debug bool) error {
	// Получаем директорию исполняемого файла
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %v", err)
	}
	exeDir := filepath.Dir(exePath)

	// Создаем папку для логов
	logsDir := filepath.Join(exeDir, "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		return fmt.Errorf("failed to create logs directory: %v", err)
	}

	// Создаем имя файла лога с текущей датой
	logFileName := fmt.Sprintf("parental-bot-%s.log", time.Now().Format("2006-01-02"))
	logFilePath := filepath.Join(logsDir, logFileName)

	// Открываем файл лога (создаем если не существует, дописываем если существует)
	logFile, err = os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %v", err)
	}

	// Настраиваем логгер
	var output io.Writer
	if debug {
		// В debug режиме пишем и в консоль, и в файл
		output = io.MultiWriter(os.Stdout, logFile)
	} else {
		// В service режиме пишем только в файл
		output = logFile
	}

	// Настраиваем стандартный логгер Go
	log.SetOutput(output)
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	// Создаем наш логгер
	defaultLog = log.New(output, "", log.Ldate|log.Ltime|log.Lshortfile)

	// Пишем начальное сообщение
	log.Printf("=== Logger initialized. Log file: %s ===", logFilePath)

	// Запускаем горутину для очистки старых логов
	go cleanOldLogs(logsDir, 7) // Храним логи за последние 7 дней

	return nil
}

// cleanOldLogs удаляет лог-файлы старше указанного количества дней
func cleanOldLogs(logsDir string, daysToKeep int) {
	ticker := time.NewTicker(24 * time.Hour) // Проверяем раз в день
	defer ticker.Stop()

	for range ticker.C {
		files, err := filepath.Glob(filepath.Join(logsDir, "parental-bot-*.log"))
		if err != nil {
			log.Printf("Failed to list log files: %v", err)
			continue
		}

		cutoffTime := time.Now().AddDate(0, 0, -daysToKeep)
		for _, file := range files {
			info, err := os.Stat(file)
			if err != nil {
				continue
			}

			if info.ModTime().Before(cutoffTime) {
				if err := os.Remove(file); err != nil {
					log.Printf("Failed to remove old log file %s: %v", file, err)
				} else {
					log.Printf("Removed old log file: %s", file)
				}
			}
		}
	}
}

// CloseLogger закрывает файл лога
func CloseLogger() {
	if logFile != nil {
		log.Println("=== Logger shutting down ===")
		logFile.Close()
	}
}

// Info логирует информационное сообщение
func Info(format string, v ...interface{}) {
	log.Printf("[INFO] "+format, v...)
}

// Error логирует сообщение об ошибке
func Error(format string, v ...interface{}) {
	log.Printf("[ERROR] "+format, v...)
}

// Warning логирует предупреждение
func Warning(format string, v ...interface{}) {
	log.Printf("[WARNING] "+format, v...)
}

// Debug логирует отладочное сообщение
func Debug(format string, v ...interface{}) {
	log.Printf("[DEBUG] "+format, v...)
}
