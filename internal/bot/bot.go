//go:build windows

package bot

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Hepri/parental/internal/config"
	"github.com/Hepri/parental/internal/session"
	"github.com/Hepri/parental/internal/shutdown"
	"github.com/Hepri/parental/internal/tracker"
)

type TelegramBot struct {
	bot               *tgbotapi.BotAPI
	config            *config.Config
	sessionMgr        *session.Manager
	tracker           *tracker.TimeTracker
	shutdownMgr       *shutdown.ShutdownManager
	userStates        map[int64]string                 // userID -> state
	userData          map[int64]map[string]interface{} // userID -> data
	reconnectAttempts int                              // Количество попыток переподключения
	isConnected       bool                             // Статус подключения
}

type BotCommand struct {
	Command     string
	Description string
	Handler     func(update tgbotapi.Update) error
}

func NewBot(cfg *config.Config, sessionMgr *session.Manager, tracker *tracker.TimeTracker, shutdownMgr *shutdown.ShutdownManager) (*TelegramBot, error) {
	// Не создаем подключение здесь - это будет сделано в connectAndRun()
	// Это позволяет создать бота даже при отсутствии интернета
	return &TelegramBot{
		bot:               nil, // Будет создан при первом подключении
		config:            cfg,
		sessionMgr:        sessionMgr,
		tracker:           tracker,
		shutdownMgr:       shutdownMgr,
		userStates:        make(map[int64]string),
		userData:          make(map[int64]map[string]interface{}),
		reconnectAttempts: 0,
		isConnected:       false,
	}, nil
}

func (tb *TelegramBot) Start(ctx context.Context) error {
	log.Printf("Starting Telegram bot with reconnect mechanism...")
	log.Printf("Reconnect settings: interval=%ds, max_attempts=%s",
		tb.config.ReconnectInterval, tb.getMaxAttemptsString())

	// Бесконечный цикл переподключения - программа не останавливается при отсутствии интернета
	for {
		select {
		case <-ctx.Done():
			log.Println("Bot context cancelled, stopping...")
			return nil
		default:
			// Пытаемся подключиться и запустить бота
			// connectAndRun() вернется при ошибке подключения/потере соединения или отмене контекста
			if err := tb.connectAndRun(ctx); err != nil {
				log.Printf("Bot connection error: %v", err)
				tb.isConnected = false

				// Всегда продолжаем попытки переподключения (бесконечно)
				// shouldReconnect() проверяет MaxReconnectAttempts, но по умолчанию оно = 0 (бесконечно)
				if !tb.shouldReconnect() {
					log.Printf("Max reconnect attempts reached (%d), but continuing anyway...", tb.config.MaxReconnectAttempts)
					// Даже если достигнут максимум, продолжаем попытки - программа не должна останавливаться
				}

				// Увеличиваем счетчик попыток
				tb.reconnectAttempts++
				log.Printf("Attempting to reconnect in %d seconds (attempt %d/%s)...",
					tb.config.ReconnectInterval,
					tb.reconnectAttempts,
					tb.getMaxAttemptsString())

				// Ждем перед следующей попыткой
				select {
				case <-ctx.Done():
					log.Println("Context cancelled during reconnect wait")
					return nil
				case <-time.After(time.Duration(tb.config.ReconnectInterval) * time.Second):
					continue
				}
			} else {
				// Если connectAndRun() вернулся без ошибки, это означает что контекст был отменен
				// и мы выходим из цикла
				log.Println("Bot stopped (context cancelled)")
				return nil
			}
		}
	}
}

func (tb *TelegramBot) Stop() {
	log.Println("Telegram bot stopped")
	tb.isConnected = false
}

// connectAndRun пытается подключиться к Telegram и запустить бота
func (tb *TelegramBot) connectAndRun(ctx context.Context) error {
	// Создаем или пересоздаем подключение к боту
	// Это работает как для начального подключения, так и для переподключений
	if tb.bot == nil || tb.reconnectAttempts > 0 {
		if tb.reconnectAttempts > 0 {
			log.Printf("Attempting to reconnect to Telegram (attempt %d)...", tb.reconnectAttempts)
		} else {
			log.Printf("Attempting initial connection to Telegram...")
		}

		// Создаем новый экземпляр бота
		bot, err := tgbotapi.NewBotAPI(tb.config.TelegramBotToken)
		if err != nil {
			return fmt.Errorf("failed to create bot connection: %v", err)
		}

		// Устанавливаем HTTP клиент с таймаутом 8 секунд для всех сетевых операций
		bot.Client = &http.Client{
			Timeout: 8 * time.Second,
		}

		tb.bot = bot
		tb.bot.Debug = false
	}

	// Проверяем подключение, получая информацию о боте
	me, err := tb.bot.GetMe()
	if err != nil {
		return fmt.Errorf("failed to verify bot connection: %v", err)
	}

	log.Printf("Telegram bot connected successfully. Bot username: @%s", me.UserName)
	tb.isConnected = true

	// Сбрасываем счетчик попыток при успешном подключении
	tb.reconnectAttempts = 0

	// Запускаем основной цикл обработки сообщений
	return tb.runMessageLoop(ctx)
}

// runMessageLoop запускает основной цикл обработки сообщений
func (tb *TelegramBot) runMessageLoop(ctx context.Context) error {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 8 // Таймаут 8 секунд для получения обновлений

	updates := tb.bot.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			log.Println("Message loop context cancelled")
			return nil
		case update, ok := <-updates:
			// Проверяем, не закрыт ли канал (это означает потерю соединения)
			if !ok {
				log.Println("Updates channel closed, connection lost")
				tb.isConnected = false
				return fmt.Errorf("updates channel closed - connection lost")
			}

			if err := tb.handleUpdate(update); err != nil {
				log.Printf("Error handling update: %v", err)
				// Если ошибка критическая, возвращаем её для переподключения
				if tb.isCriticalError(err) {
					tb.isConnected = false
					return err
				}
			}
		}
	}
}

// shouldReconnect определяет, нужно ли продолжать попытки переподключения
func (tb *TelegramBot) shouldReconnect() bool {
	// Если MaxReconnectAttempts = 0, то бесконечные попытки
	if tb.config.MaxReconnectAttempts == 0 {
		return true
	}

	// Проверяем, не превышено ли максимальное количество попыток
	return tb.reconnectAttempts < tb.config.MaxReconnectAttempts
}

// getMaxAttemptsString возвращает строковое представление максимального количества попыток
func (tb *TelegramBot) getMaxAttemptsString() string {
	if tb.config.MaxReconnectAttempts == 0 {
		return "∞"
	}
	return fmt.Sprintf("%d", tb.config.MaxReconnectAttempts)
}

// isCriticalError определяет, является ли ошибка критической для переподключения
func (tb *TelegramBot) isCriticalError(err error) bool {
	if err == nil {
		return false
	}

	errStr := strings.ToLower(err.Error())

	// Критические ошибки, требующие переподключения
	criticalErrors := []string{
		"connection refused",
		"connection reset",
		"timeout",
		"network is unreachable",
		"no route to host",
		"connection lost",
		"telegram api error",
		"unauthorized",
		"forbidden",
		"no such host",
		"i/o timeout",
		"context deadline exceeded",
		"connection closed",
		"broken pipe",
		"eof",
		"network",
		"dial tcp",
		"read: connection",
		"write: broken pipe",
	}

	for _, criticalErr := range criticalErrors {
		if strings.Contains(errStr, criticalErr) {
			return true
		}
	}

	return false
}

// GetMe returns bot information for testing
func (tb *TelegramBot) GetMe() (tgbotapi.User, error) {
	if tb.bot == nil {
		return tgbotapi.User{}, fmt.Errorf("bot not connected")
	}
	return tb.bot.GetMe()
}

func (tb *TelegramBot) handleUpdate(update tgbotapi.Update) error {
	if update.Message == nil && update.CallbackQuery == nil {
		return nil
	}

	var userID int64
	var chatID int64

	if update.Message != nil {
		userID = update.Message.From.ID
		chatID = update.Message.Chat.ID
	} else if update.CallbackQuery != nil {
		userID = update.CallbackQuery.From.ID
		chatID = update.CallbackQuery.Message.Chat.ID
	}

	// Check authorization
	if !tb.isAuthorized(userID) {
		msg := tgbotapi.NewMessage(chatID, "⛔ Доступ запрещён. Этот бот предназначен только для авторизованных родителей.")
		tb.bot.Send(msg)
		return nil
	}

	// Handle callback queries
	if update.CallbackQuery != nil {
		return tb.handleCallbackQuery(update.CallbackQuery)
	}

	// Handle text messages
	if update.Message != nil {
		return tb.handleMessage(update.Message)
	}

	return nil
}

func (tb *TelegramBot) isAuthorized(userID int64) bool {
	for _, authorizedID := range tb.config.AuthorizedUserIDs {
		if userID == authorizedID {
			return true
		}
	}
	return false
}

func (tb *TelegramBot) handleMessage(message *tgbotapi.Message) error {
	text := message.Text
	chatID := message.Chat.ID

	switch text {
	case "/start":
		return tb.showMainMenu(chatID)
	default:
		// Check if user is in a state that expects input
		if state, exists := tb.userStates[message.From.ID]; exists {
			return tb.handleStateInput(message, state)
		}

		// Unknown command
		msg := tgbotapi.NewMessage(chatID, "Неизвестная команда. Используйте /start, чтобы открыть главное меню.")
		tb.bot.Send(msg)
		return nil
	}
}

func (tb *TelegramBot) handleCallbackQuery(query *tgbotapi.CallbackQuery) error {
	data := query.Data
	chatID := query.Message.Chat.ID
	messageID := query.Message.MessageID

	// Answer callback query
	callback := tgbotapi.NewCallback(query.ID, "")
	tb.bot.Request(callback)

	switch {
	case data == "lock_all":
		return tb.handleLockAllNow(chatID, messageID)
	case strings.HasPrefix(data, "grant_"):
		return tb.handleGrantAccess(data, chatID, messageID)
	case strings.HasPrefix(data, "duration_"):
		return tb.handleDurationSelection(data, chatID, messageID)
	case strings.HasPrefix(data, "lock_"):
		return tb.handleLockSession(data, chatID, messageID)
	case strings.HasPrefix(data, "extend_"):
		return tb.handleExtendSession(data, chatID, messageID)
	case data == "resetpw_all":
		return tb.handleResetAllPasswords(chatID, messageID)
	case strings.HasPrefix(data, "resetpw_"):
		return tb.handleResetPassword(data, chatID, messageID)
	case data == "stats_menu":
		return tb.showStatsMenu(chatID, messageID)
	case data == "stats_today":
		return tb.showTodayStats(chatID, messageID)
	case data == "stats_week":
		return tb.showWeekStats(chatID, messageID)
	case data == "computer_menu":
		return tb.showComputerMenu(chatID, messageID)
	case data == "computer_status":
		return tb.showComputerStatus(chatID, messageID)
	case data == "shutdown_now":
		return tb.shutdownNow(chatID, messageID)
	case strings.HasPrefix(data, "shutdown_"):
		return tb.scheduleShutdown(data, chatID, messageID)
	case data == "cancel_shutdown":
		return tb.cancelShutdown(chatID, messageID)
	case data == "resetpw_menu":
		return tb.showResetPasswordMenu(chatID, messageID)
	case data == "main_menu":
		return tb.showMainMenu(chatID)
	default:
		return nil
	}
}

func (tb *TelegramBot) showMainMenu(chatID int64) error {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🟢 Выдать доступ", "grant_menu"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔒 Завершить сеанс", "lock_all"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔁 Сбросить пароль", "resetpw_menu"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Статистика", "stats_menu"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Управление компьютером", "computer_menu"),
		),
	)

	msg := tgbotapi.NewMessage(chatID, "🏠 *Родительский контроль*\n\nВыберите действие:")
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = keyboard

	_, err := tb.bot.Send(msg)
	return err
}

func (tb *TelegramBot) showResetPasswordMenu(chatID int64, messageID int) error {
	var buttons [][]tgbotapi.InlineKeyboardButton

	// Add "reset all" action first
	buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🔁 Сбросить пароли всех", "resetpw_all"),
	))

	for _, account := range tb.config.ChildAccounts {
		buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(account.FullName, "resetpw_"+account.Username),
		))
	}

	buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu"),
	))

	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)

	if messageID > 0 {
		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, "🔁 *Сброс пароля*\n\nВыберите аккаунт ребёнка для восстановления пароля из конфигурации:")
		editMsg.ParseMode = "Markdown"
		editMsg.ReplyMarkup = &keyboard
		_, err := tb.bot.Send(editMsg)
		return err
	}

	msg := tgbotapi.NewMessage(chatID, "🔁 *Сброс пароля*\n\nВыберите аккаунт ребёнка для восстановления пароля из конфигурации:")
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = keyboard
	_, err := tb.bot.Send(msg)
	return err
}

func (tb *TelegramBot) handleResetPassword(data string, chatID int64, messageID int) error {
	// data format: resetpw_<username>
	username := strings.TrimPrefix(data, "resetpw_")

	var configured string
	for _, acc := range tb.config.ChildAccounts {
		if acc.Username == username {
			configured = acc.Password
			break
		}
	}

	if configured == "" {
		// Fallback: reset all child passwords
		return tb.handleResetAllPasswords(chatID, messageID)
	}

	if err := config.SetUserPassword(username, configured); err != nil {
		msg := tgbotapi.NewEditMessageText(chatID, messageID, fmt.Sprintf("❌ Не удалось сбросить пароль для %s: %v", username, err))
		tb.bot.Send(msg)
		return err
	}

	msg := tgbotapi.NewEditMessageText(chatID, messageID, fmt.Sprintf("✅ Пароль для %s успешно восстановлен.", username))
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu")},
		},
	}
	_, err := tb.bot.Send(msg)
	return err
}

func (tb *TelegramBot) handleResetAllPasswords(chatID int64, messageID int) error {
	total := len(tb.config.ChildAccounts)
	success := 0
	failed := 0
	for _, acc := range tb.config.ChildAccounts {
		if acc.Password == "" {
			// Skip accounts without configured password
			failed++
			continue
		}
		if err := config.SetUserPassword(acc.Username, acc.Password); err != nil {
			failed++
		} else {
			success++
		}
	}
	text := fmt.Sprintf("✅ Сброс паролей завершён. Успешно: %d из %d.", success, total)
	if failed > 0 {
		text = fmt.Sprintf("✅ Сброс паролей завершён. Успешно: %d из %d. Не удалось: %d.", success, total, failed)
	}
	msg := tgbotapi.NewEditMessageText(chatID, messageID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu")},
		},
	}
	_, err := tb.bot.Send(msg)
	return err
}

func (tb *TelegramBot) handleGrantAccess(data string, chatID int64, messageID int) error {
	if data == "grant_menu" {
		return tb.showGrantAccessMenu(chatID, messageID)
	}

	// Extract username from callback data
	username := strings.TrimPrefix(data, "grant_")

	// Set user state and data
	tb.userStates[chatID] = "grant_duration"
	tb.userData[chatID] = map[string]interface{}{
		"selected_user": username,
	}

	return tb.showDurationMenu(chatID, messageID)
}

func (tb *TelegramBot) showGrantAccessMenu(chatID int64, messageID int) error {
	var buttons [][]tgbotapi.InlineKeyboardButton

	for _, account := range tb.config.ChildAccounts {
		buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(account.FullName, "grant_"+account.Username),
		))
	}

	buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🔙 Назад в главное меню", "main_menu"),
	))

	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)

	if messageID > 0 {
		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, "👤 *Выбор аккаунта ребёнка*\n\nКому выдать доступ?")
		editMsg.ParseMode = "Markdown"
		editMsg.ReplyMarkup = &keyboard
		_, err := tb.bot.Send(editMsg)
		return err
	}

	msg := tgbotapi.NewMessage(chatID, "👤 *Выбор аккаунта ребёнка*\n\nКому выдать доступ?")
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = keyboard
	_, err := tb.bot.Send(msg)
	return err
}

func (tb *TelegramBot) showDurationMenu(chatID int64, messageID int) error {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("15 минут", "duration_15"),
			tgbotapi.NewInlineKeyboardButtonData("30 минут", "duration_30"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("1 час", "duration_60"),
			tgbotapi.NewInlineKeyboardButtonData("2 часа", "duration_120"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Другая длительность", "duration_custom"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "grant_menu"),
		),
	)

	userData, ok := tb.userData[chatID]
	if !ok {
		tb.userStates[chatID] = "grant_duration"
		return tb.showGrantAccessMenu(chatID, messageID)
	}
	username, ok := userData["selected_user"].(string)
	if !ok || username == "" {
		tb.userStates[chatID] = "grant_duration"
		return tb.showGrantAccessMenu(chatID, messageID)
	}

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, fmt.Sprintf("⏰ *Выбор длительности*\n\nПользователь: *%s*\n\nНа сколько выдать доступ?", username))
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &keyboard

	_, err := tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) handleDurationSelection(data string, chatID int64, messageID int) error {
	if data == "duration_custom" {
		tb.userStates[chatID] = "custom_duration"
		msg := tgbotapi.NewEditMessageText(chatID, messageID, "⌨️ *Своя длительность*\n\nВведите длительность в минутах (1–480):")
		msg.ParseMode = "Markdown"
		msg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
				{tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "grant_menu")},
			},
		}
		_, err := tb.bot.Send(msg)
		return err
	}

	// Extract duration from callback data
	durationStr := strings.TrimPrefix(data, "duration_")
	duration, err := strconv.Atoi(durationStr)
	if err != nil {
		return err
	}

	return tb.grantAccess(chatID, messageID, duration)
}

func (tb *TelegramBot) handleStateInput(message *tgbotapi.Message, state string) error {
	chatID := message.Chat.ID
	text := message.Text

	switch state {
	case "custom_duration":
		duration, err := strconv.Atoi(text)
		if err != nil || duration < 1 || duration > 480 {
			msg := tgbotapi.NewMessage(chatID, "❌ Некорректная длительность. Введите число от 1 до 480 минут.")
			tb.bot.Send(msg)
			return nil
		}

		// Clear state
		delete(tb.userStates, message.From.ID)

		return tb.grantAccess(chatID, 0, duration)
	}

	return nil
}

func (tb *TelegramBot) grantAccess(chatID int64, messageID int, durationMinutes int) error {
	userData, ok := tb.userData[chatID]
	if !ok {
		// guide user to select child first
		_ = tb.showGrantAccessMenu(chatID, messageID)
		return fmt.Errorf("no child selected")
	}
	username, ok := userData["selected_user"].(string)
	if !ok || username == "" {
		_ = tb.showGrantAccessMenu(chatID, messageID)
		return fmt.Errorf("no child selected")
	}

	duration := time.Duration(durationMinutes) * time.Minute

	err := tb.sessionMgr.GrantAccess(username, duration)
	if err != nil {
		msgText := fmt.Sprintf("❌ Не удалось выдать доступ для %s: %v", username, err)
		if messageID > 0 {
			editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
			tb.bot.Send(editMsg)
		} else {
			msg := tgbotapi.NewMessage(chatID, msgText)
			tb.bot.Send(msg)
		}
		return err
	}

	// Clear user data
	delete(tb.userData, chatID)

	msgText := fmt.Sprintf("✅ *Доступ выдан*\n\n👤 Пользователь: %s\n⏰ Длительность: %d мин\n\nПо окончании времени сеанс будет завершён, а пароль — восстановлен.", username, durationMinutes)
	if messageID > 0 {
		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
		editMsg.ParseMode = "Markdown"
		editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
				{tgbotapi.NewInlineKeyboardButtonData("🔒 Завершить сейчас", "lock_"+username)},
				{tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu")},
			},
		}
		_, err = tb.bot.Send(editMsg)
	} else {
		msg := tgbotapi.NewMessage(chatID, msgText)
		msg.ParseMode = "Markdown"
		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔒 Завершить сейчас", "lock_"+username),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu"),
			),
		)
		_, err = tb.bot.Send(msg)
	}

	return err
}

func (tb *TelegramBot) handleLockSession(data string, chatID int64, messageID int) error {
	if data == "lock_menu" {
		return tb.showLockMenu(chatID, messageID)
	}

	username := strings.TrimPrefix(data, "lock_")

	err := tb.sessionMgr.LockSession(username)
	if err != nil {
		msgText := fmt.Sprintf("❌ Не удалось завершить сеанс %s: %v", username, err)
		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
		tb.bot.Send(editMsg)
		return err
	}

	msgText := fmt.Sprintf("🔒 *Экран заблокирован*\n\nПользователь %s заблокирован, пароль восстановлен.", username)
	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu")},
		},
	}

	_, err = tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) handleExtendSession(data string, chatID int64, messageID int) error {
	username := strings.TrimPrefix(data, "extend_")
	if tb.sessionMgr == nil {
		return nil
	}
	// Extend by 15 minutes
	if err := tb.sessionMgr.ExtendSession(username, 15*time.Minute); err != nil {
		msg := tgbotapi.NewEditMessageText(chatID, messageID, fmt.Sprintf("❌ Не удалось продлить сеанс для %s: %v", username, err))
		tb.bot.Send(msg)
		return err
	}
	msg := tgbotapi.NewEditMessageText(chatID, messageID, fmt.Sprintf("✅ Сеанс %s продлён на 15 минут.", username))
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu")},
		},
	}
	_, err := tb.bot.Send(msg)
	return err
}

func (tb *TelegramBot) handleLockAllNow(chatID int64, messageID int) error {
	if tb.sessionMgr == nil {
		return nil
	}
	if err := tb.sessionMgr.ForceLogoffAllChildSessions(); err != nil {
		msg := tgbotapi.NewEditMessageText(chatID, messageID, fmt.Sprintf("❌ Не удалось завершить все сеансы: %v", err))
		tb.bot.Send(msg)
		return err
	}
	msg := tgbotapi.NewEditMessageText(chatID, messageID, "🔒 Все детские сеансы заблокированы, пароли восстановлены.")
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu")},
		},
	}
	_, err := tb.bot.Send(msg)
	return err
}

func (tb *TelegramBot) showLockMenu(chatID int64, messageID int) error {
	activeSessions := tb.sessionMgr.GetActiveSessions()

	var buttons [][]tgbotapi.InlineKeyboardButton

	if len(activeSessions) == 0 {
		msgText := "🔒 *Сеансы*\n\nАктивные сеансы не найдены."
		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
		editMsg.ParseMode = "Markdown"
		editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
				{tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu")},
			},
		}
		_, err := tb.bot.Send(editMsg)
		return err
	}

	for username, session := range activeSessions {
		remaining := session.Duration - time.Since(session.StartTime)
		buttonText := fmt.Sprintf("🔒 %s (осталось %v)", username, remaining.Round(time.Minute))
		buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(buttonText, "lock_"+username),
			tgbotapi.NewInlineKeyboardButtonData("➕ +15 мин", "extend_"+username),
		))
	}

	if len(activeSessions) > 1 {
		buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔒 Завершить все", "lock_all"),
		))
	}

	buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu"),
	))

	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, "🔒 *Сеансы*\n\nВыберите сеанс для завершения:")
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &keyboard

	_, err := tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) showStatsMenu(chatID int64, messageID int) error {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Отчёт за сегодня", "stats_today"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Отчёт за неделю", "stats_week"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu"),
		),
	)

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, "📊 *Статистика*\n\nВыберите период для отчёта:")
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &keyboard

	_, err := tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) showComputerMenu(chatID int64, messageID int) error {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("💻 Состояние", "computer_status"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔴 Выключить сейчас", "shutdown_now"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⏰ Запланировать выключение", "shutdown_menu"),
		),
	)

	if tb.shutdownMgr.IsShutdownScheduled() {
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard,
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("❌ Отменить выключение", "cancel_shutdown"),
			),
		)
	}

	keyboard.InlineKeyboard = append(keyboard.InlineKeyboard,
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu"),
		),
	)

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, "⚙️ *Управление компьютером*\n\nВыберите действие:")
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &keyboard

	_, err := tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) showTodayStats(chatID int64, messageID int) error {
	report := tb.tracker.GetTodayReport()

	if len(report) == 0 {
		msgText := "📊 *Отчёт за сегодня*\n\nДанных об активности нет."
		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
		editMsg.ParseMode = "Markdown"
		editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
				{tgbotapi.NewInlineKeyboardButtonData("📊 За неделю", "stats_week")},
				{tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu")},
			},
		}
		_, err := tb.bot.Send(editMsg)
		return err
	}

	var msgText strings.Builder
	msgText.WriteString("📊 *Отчёт за сегодня*\n\n")

	totalTime := int64(0)
	for app, seconds := range report {
		totalTime += seconds
		minutes := seconds / 60
		msgText.WriteString(fmt.Sprintf("• %s: %d мин\n", app, minutes))
	}

	msgText.WriteString(fmt.Sprintf("\n📈 Итого: %d мин", totalTime/60))

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText.String())
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("📊 За неделю", "stats_week")},
			{tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu")},
		},
	}

	_, err := tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) showWeekStats(chatID int64, messageID int) error {
	report := tb.tracker.GetWeekReport()

	if len(report) == 0 {
		msgText := "📊 *Отчёт за неделю*\n\nДанных об активности нет."
		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
		editMsg.ParseMode = "Markdown"
		editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
				{tgbotapi.NewInlineKeyboardButtonData("📊 За сегодня", "stats_today")},
				{tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu")},
			},
		}
		_, err := tb.bot.Send(editMsg)
		return err
	}

	var msgText strings.Builder
	msgText.WriteString("📊 *Отчёт за неделю*\n\n")

	totalTime := int64(0)
	for app, seconds := range report {
		totalTime += seconds
		minutes := seconds / 60
		msgText.WriteString(fmt.Sprintf("• %s: %d мин\n", app, minutes))
	}

	msgText.WriteString(fmt.Sprintf("\n📈 Итого: %d мин", totalTime/60))

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText.String())
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("📊 За сегодня", "stats_today")},
			{tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu")},
		},
	}

	_, err := tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) showComputerStatus(chatID int64, messageID int) error {
	activeSessions := tb.sessionMgr.GetActiveSessions()

	var msgText strings.Builder
	msgText.WriteString("💻 *Состояние компьютера*\n\n")

	if len(activeSessions) == 0 {
		msgText.WriteString("🔒 Активных сеансов нет\n")
	} else {
		msgText.WriteString("🟢 Активные сеансы:\n")
		for username, session := range activeSessions {
			remaining := session.Duration - time.Since(session.StartTime)
			msgText.WriteString(fmt.Sprintf("• %s: осталось %v\n", username, remaining.Round(time.Minute)))
		}
	}

	if tb.shutdownMgr.IsShutdownScheduled() {
		scheduledTime := tb.shutdownMgr.GetScheduledTime()
		msgText.WriteString(fmt.Sprintf("\n⏰ Выключение запланировано: %s", scheduledTime.Format("15:04")))
	}

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText.String())
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("🔴 Выключить сейчас", "shutdown_now")},
			{tgbotapi.NewInlineKeyboardButtonData("⏰ Запланировать выключение", "shutdown_menu")},
			{tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu")},
		},
	}

	_, err := tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) shutdownNow(chatID int64, messageID int) error {
	err := tb.shutdownMgr.ShutdownNow()
	if err != nil {
		msgText := fmt.Sprintf("❌ *Не удалось выключить*\n\nОшибка: %v", err)
		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
		editMsg.ParseMode = "Markdown"
		editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
				{tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu")},
			},
		}
		_, err = tb.bot.Send(editMsg)
		return err
	}

	msgText := "🔴 *Выключение инициировано*\n\nКомпьютер выключится через 30 секунд."

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("❌ Отменить выключение", "cancel_shutdown")},
			{tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu")},
		},
	}

	_, err = tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) scheduleShutdown(data string, chatID int64, messageID int) error {
	if data == "shutdown_menu" {
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("5 минут", "shutdown_5"),
				tgbotapi.NewInlineKeyboardButtonData("15 минут", "shutdown_15"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("30 минут", "shutdown_30"),
				tgbotapi.NewInlineKeyboardButtonData("1 час", "shutdown_60"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu"),
			),
		)

		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, "⏰ *Запланировать выключение*\n\nВыберите, через сколько минут выключить компьютер:")
		editMsg.ParseMode = "Markdown"
		editMsg.ReplyMarkup = &keyboard

		_, err := tb.bot.Send(editMsg)
		return err
	}

	// parse minutes
	minsStr := strings.TrimPrefix(data, "shutdown_")
	mins, err := strconv.Atoi(minsStr)
	if err != nil {
		return err
	}

	err = tb.shutdownMgr.ScheduleShutdown(mins)
	if err != nil {
		msgText := fmt.Sprintf("❌ *Не удалось запланировать выключение*\n\nОшибка: %v", err)
		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
		editMsg.ParseMode = "Markdown"
		editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
				{tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu")},
			},
		}
		_, err = tb.bot.Send(editMsg)
		return err
	}

	msgText := fmt.Sprintf("⏰ *Выключение запланировано*\n\nКомпьютер выключится через %d минут.", mins)
	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("❌ Отменить выключение", "cancel_shutdown")},
			{tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu")},
		},
	}
	_, err = tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) cancelShutdown(chatID int64, messageID int) error {
	err := tb.shutdownMgr.CancelShutdown()
	if err != nil {
		msgText := fmt.Sprintf("❌ *Не удалось отменить выключение*\n\nОшибка: %v", err)
		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
		editMsg.ParseMode = "Markdown"
		editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
				{tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu")},
			},
		}
		_, err = tb.bot.Send(editMsg)
		return err
	}

	msgText := "❌ *Выключение отменено*."
	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "main_menu")},
		},
	}
	_, err = tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) NotifySessionExpired(username string) {
	// Проверяем, что бот подключен перед отправкой уведомлений
	if tb.bot == nil || !tb.isConnected {
		log.Printf("Cannot notify about expired session for %s: bot not connected", username)
		return
	}

	// Notify all authorized users about expired session
	for _, userID := range tb.config.AuthorizedUserIDs {
		msg := tgbotapi.NewMessage(userID, fmt.Sprintf("⏰ *Сеанс истек*\n\nСессия пользователя %s истекла и заблокирована.", username))
		msg.ParseMode = "Markdown"
		if _, err := tb.bot.Send(msg); err != nil {
			log.Printf("Failed to send session expired notification to user %d: %v", userID, err)
			// Если ошибка критическая, помечаем соединение как потерянное
			if tb.isCriticalError(err) {
				tb.isConnected = false
			}
		}
	}
}
