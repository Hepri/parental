//go:build windows

package bot

import (
	"context"
	"fmt"
	"log"
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
	bot         *tgbotapi.BotAPI
	config      *config.Config
	sessionMgr  *session.Manager
	tracker     *tracker.TimeTracker
	shutdownMgr *shutdown.ShutdownManager
	userStates  map[int64]string                 // userID -> state
	userData    map[int64]map[string]interface{} // userID -> data
}

type BotCommand struct {
	Command     string
	Description string
	Handler     func(update tgbotapi.Update) error
}

func NewBot(cfg *config.Config, sessionMgr *session.Manager, tracker *tracker.TimeTracker, shutdownMgr *shutdown.ShutdownManager) (*TelegramBot, error) {
	bot, err := tgbotapi.NewBotAPI(cfg.TelegramBotToken)
	if err != nil {
		return nil, fmt.Errorf("failed to create bot: %v", err)
	}

	bot.Debug = false // Set to true for debugging

	return &TelegramBot{
		bot:         bot,
		config:      cfg,
		sessionMgr:  sessionMgr,
		tracker:     tracker,
		shutdownMgr: shutdownMgr,
		userStates:  make(map[int64]string),
		userData:    make(map[int64]map[string]interface{}),
	}, nil
}

func (tb *TelegramBot) Start(ctx context.Context) error {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := tb.bot.GetUpdatesChan(u)

	log.Printf("Telegram bot started. Bot username: @%s", tb.bot.Self.UserName)

	for {
		select {
		case <-ctx.Done():
			return nil
		case update := <-updates:
			if err := tb.handleUpdate(update); err != nil {
				log.Printf("Error handling update: %v", err)
			}
		}
	}
}

func (tb *TelegramBot) Stop() {
	log.Println("Telegram bot stopped")
}

// GetMe returns bot information for testing
func (tb *TelegramBot) GetMe() (tgbotapi.User, error) {
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
	case strings.HasPrefix(data, "grant_"):
		return tb.handleGrantAccess(data, chatID, messageID)
	case strings.HasPrefix(data, "duration_"):
		return tb.handleDurationSelection(data, chatID, messageID)
	case strings.HasPrefix(data, "lock_"):
		return tb.handleLockSession(data, chatID, messageID)
	case data == "lock_all":
		return tb.handleLockAllNow(chatID, messageID)
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

	msgText := fmt.Sprintf("🔒 *Сеанс завершён*\n\nПользователь %s вышел из системы, пароль восстановлен.", username)
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

func (tb *TelegramBot) handleLockAllNow(chatID int64, messageID int) error {
	if tb.sessionMgr == nil {
		return nil
	}
	if err := tb.sessionMgr.ForceLogoffAllChildSessions(); err != nil {
		msg := tgbotapi.NewEditMessageText(chatID, messageID, fmt.Sprintf("❌ Не удалось завершить все сеансы: %v", err))
		tb.bot.Send(msg)
		return err
	}
	msg := tgbotapi.NewEditMessageText(chatID, messageID, "🔒 Все детские сеансы завершены, пароли восстановлены.")
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
	// Notify all authorized users about expired session
	for _, userID := range tb.config.AuthorizedUserIDs {
		msg := tgbotapi.NewMessage(userID, fmt.Sprintf("⏰ *Сеанс истек*\n\nСессия пользователя %s истекла и заблокирована.", username))
		msg.ParseMode = "Markdown"
		tb.bot.Send(msg)
	}
}
