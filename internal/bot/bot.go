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
		msg := tgbotapi.NewMessage(chatID, "⛔ Access denied. This bot is for authorized parents only.")
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
		msg := tgbotapi.NewMessage(chatID, "Unknown command. Use /start to see the main menu.")
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
	case data == "main_menu":
		return tb.showMainMenu(chatID)
	default:
		return nil
	}
}

func (tb *TelegramBot) showMainMenu(chatID int64) error {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🟢 Grant Access", "grant_menu"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔒 Lock Session", "lock_menu"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 View Statistics", "stats_menu"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Computer Control", "computer_menu"),
		),
	)

	msg := tgbotapi.NewMessage(chatID, "🏠 *Parental Control Bot*\n\nSelect an option:")
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = keyboard

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
		tgbotapi.NewInlineKeyboardButtonData("🔙 Back to Main Menu", "main_menu"),
	))

	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)

	if messageID > 0 {
		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, "👤 *Select Child Account*\n\nChoose which child to grant access to:")
		editMsg.ParseMode = "Markdown"
		editMsg.ReplyMarkup = &keyboard
		_, err := tb.bot.Send(editMsg)
		return err
	}

	msg := tgbotapi.NewMessage(chatID, "👤 *Select Child Account*\n\nChoose which child to grant access to:")
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = keyboard
	_, err := tb.bot.Send(msg)
	return err
}

func (tb *TelegramBot) showDurationMenu(chatID int64, messageID int) error {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("15 minutes", "duration_15"),
			tgbotapi.NewInlineKeyboardButtonData("30 minutes", "duration_30"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("1 hour", "duration_60"),
			tgbotapi.NewInlineKeyboardButtonData("2 hours", "duration_120"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Custom", "duration_custom"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔙 Back", "grant_menu"),
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

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, fmt.Sprintf("⏰ *Select Duration*\n\nGranting access to: *%s*\n\nHow long should the session last?", username))
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &keyboard

	_, err := tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) handleDurationSelection(data string, chatID int64, messageID int) error {
	if data == "duration_custom" {
		tb.userStates[chatID] = "custom_duration"
		msg := tgbotapi.NewEditMessageText(chatID, messageID, "⌨️ *Custom Duration*\n\nPlease enter the duration in minutes (1-480):")
		msg.ParseMode = "Markdown"
		msg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
				{tgbotapi.NewInlineKeyboardButtonData("🔙 Back", "grant_menu")},
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
			msg := tgbotapi.NewMessage(chatID, "❌ Invalid duration. Please enter a number between 1 and 480 minutes.")
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
		msgText := fmt.Sprintf("❌ Failed to grant access to %s: %v", username, err)
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

	msgText := fmt.Sprintf("✅ *Access Granted*\n\n👤 User: %s\n⏰ Duration: %d minutes\n\nSession will automatically lock after the time expires.", username, durationMinutes)
	if messageID > 0 {
		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
		editMsg.ParseMode = "Markdown"
		editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
				{tgbotapi.NewInlineKeyboardButtonData("🔒 Lock Now", "lock_"+username)},
				{tgbotapi.NewInlineKeyboardButtonData("🏠 Main Menu", "main_menu")},
			},
		}
		_, err = tb.bot.Send(editMsg)
	} else {
		msg := tgbotapi.NewMessage(chatID, msgText)
		msg.ParseMode = "Markdown"
		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔒 Lock Now", "lock_"+username),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🏠 Main Menu", "main_menu"),
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
		msgText := fmt.Sprintf("❌ Failed to lock session for %s: %v", username, err)
		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
		tb.bot.Send(editMsg)
		return err
	}

	msgText := fmt.Sprintf("🔒 *Session Locked*\n\nUser %s has been locked out.", username)
	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("🏠 Main Menu", "main_menu")},
		},
	}

	_, err = tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) showLockMenu(chatID int64, messageID int) error {
	activeSessions := tb.sessionMgr.GetActiveSessions()

	var buttons [][]tgbotapi.InlineKeyboardButton

	if len(activeSessions) == 0 {
		msgText := "🔒 *Lock Sessions*\n\nNo active sessions found."
		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
		editMsg.ParseMode = "Markdown"
		editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
				{tgbotapi.NewInlineKeyboardButtonData("🏠 Main Menu", "main_menu")},
			},
		}
		_, err := tb.bot.Send(editMsg)
		return err
	}

	for username, session := range activeSessions {
		remaining := session.Duration - time.Since(session.StartTime)
		buttonText := fmt.Sprintf("🔒 %s (%v remaining)", username, remaining.Round(time.Minute))
		buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(buttonText, "lock_"+username),
		))
	}

	if len(activeSessions) > 1 {
		buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔒 Lock All", "lock_all"),
		))
	}

	buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🏠 Main Menu", "main_menu"),
	))

	keyboard := tgbotapi.NewInlineKeyboardMarkup(buttons...)

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, "🔒 *Lock Sessions*\n\nSelect a session to lock:")
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &keyboard

	_, err := tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) showStatsMenu(chatID int64, messageID int) error {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Today's Report", "stats_today"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 This Week's Report", "stats_week"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🏠 Main Menu", "main_menu"),
		),
	)

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, "📊 *View Statistics*\n\nSelect a time period to view activity reports:")
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &keyboard

	_, err := tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) showComputerMenu(chatID int64, messageID int) error {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("💻 Status", "computer_status"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔴 Shutdown Now", "shutdown_now"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⏰ Schedule Shutdown", "shutdown_menu"),
		),
	)

	if tb.shutdownMgr.IsShutdownScheduled() {
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard,
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("❌ Cancel Shutdown", "cancel_shutdown"),
			),
		)
	}

	keyboard.InlineKeyboard = append(keyboard.InlineKeyboard,
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🏠 Main Menu", "main_menu"),
		),
	)

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, "⚙️ *Computer Control*\n\nSelect an option:")
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &keyboard

	_, err := tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) showTodayStats(chatID int64, messageID int) error {
	report := tb.tracker.GetTodayReport()

	if len(report) == 0 {
		msgText := "📊 *Today's Report*\n\nNo activity recorded today."
		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
		editMsg.ParseMode = "Markdown"
		editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
				{tgbotapi.NewInlineKeyboardButtonData("📊 This Week", "stats_week")},
				{tgbotapi.NewInlineKeyboardButtonData("🏠 Main Menu", "main_menu")},
			},
		}
		_, err := tb.bot.Send(editMsg)
		return err
	}

	var msgText strings.Builder
	msgText.WriteString("📊 *Today's Report*\n\n")

	totalTime := int64(0)
	for app, seconds := range report {
		totalTime += seconds
		minutes := seconds / 60
		msgText.WriteString(fmt.Sprintf("• %s: %d minutes\n", app, minutes))
	}

	msgText.WriteString(fmt.Sprintf("\n📈 Total: %d minutes", totalTime/60))

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText.String())
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("📊 This Week", "stats_week")},
			{tgbotapi.NewInlineKeyboardButtonData("🏠 Main Menu", "main_menu")},
		},
	}

	_, err := tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) showWeekStats(chatID int64, messageID int) error {
	report := tb.tracker.GetWeekReport()

	if len(report) == 0 {
		msgText := "📊 *This Week's Report*\n\nNo activity recorded this week."
		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
		editMsg.ParseMode = "Markdown"
		editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
				{tgbotapi.NewInlineKeyboardButtonData("📊 Today", "stats_today")},
				{tgbotapi.NewInlineKeyboardButtonData("🏠 Main Menu", "main_menu")},
			},
		}
		_, err := tb.bot.Send(editMsg)
		return err
	}

	var msgText strings.Builder
	msgText.WriteString("📊 *This Week's Report*\n\n")

	totalTime := int64(0)
	for app, seconds := range report {
		totalTime += seconds
		minutes := seconds / 60
		msgText.WriteString(fmt.Sprintf("• %s: %d minutes\n", app, minutes))
	}

	msgText.WriteString(fmt.Sprintf("\n📈 Total: %d minutes", totalTime/60))

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText.String())
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("📊 Today", "stats_today")},
			{tgbotapi.NewInlineKeyboardButtonData("🏠 Main Menu", "main_menu")},
		},
	}

	_, err := tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) showComputerStatus(chatID int64, messageID int) error {
	activeSessions := tb.sessionMgr.GetActiveSessions()

	var msgText strings.Builder
	msgText.WriteString("💻 *Computer Status*\n\n")

	if len(activeSessions) == 0 {
		msgText.WriteString("🔒 No active sessions\n")
	} else {
		msgText.WriteString("🟢 Active Sessions:\n")
		for username, session := range activeSessions {
			remaining := session.Duration - time.Since(session.StartTime)
			msgText.WriteString(fmt.Sprintf("• %s: %v remaining\n", username, remaining.Round(time.Minute)))
		}
	}

	if tb.shutdownMgr.IsShutdownScheduled() {
		scheduledTime := tb.shutdownMgr.GetScheduledTime()
		msgText.WriteString(fmt.Sprintf("\n⏰ Shutdown scheduled: %s", scheduledTime.Format("15:04")))
	}

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText.String())
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("🔴 Shutdown Now", "shutdown_now")},
			{tgbotapi.NewInlineKeyboardButtonData("⏰ Schedule Shutdown", "shutdown_menu")},
			{tgbotapi.NewInlineKeyboardButtonData("🏠 Main Menu", "main_menu")},
		},
	}

	_, err := tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) shutdownNow(chatID int64, messageID int) error {
	err := tb.shutdownMgr.ShutdownNow()
	if err != nil {
		msgText := fmt.Sprintf("❌ *Shutdown Failed*\n\nError: %v", err)
		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
		editMsg.ParseMode = "Markdown"
		editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
				{tgbotapi.NewInlineKeyboardButtonData("🏠 Main Menu", "main_menu")},
			},
		}
		_, err = tb.bot.Send(editMsg)
		return err
	}

	msgText := "🔴 *Shutdown Initiated*\n\nThe computer will shutdown in 30 seconds."

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("❌ Cancel Shutdown", "cancel_shutdown")},
			{tgbotapi.NewInlineKeyboardButtonData("🏠 Main Menu", "main_menu")},
		},
	}

	_, err = tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) scheduleShutdown(data string, chatID int64, messageID int) error {
	if data == "shutdown_menu" {
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("5 minutes", "shutdown_5"),
				tgbotapi.NewInlineKeyboardButtonData("15 minutes", "shutdown_15"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("30 minutes", "shutdown_30"),
				tgbotapi.NewInlineKeyboardButtonData("1 hour", "shutdown_60"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🏠 Main Menu", "main_menu"),
			),
		)

		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, "⏰ *Schedule Shutdown*\n\nWhen should the computer shutdown?")
		editMsg.ParseMode = "Markdown"
		editMsg.ReplyMarkup = &keyboard

		_, err := tb.bot.Send(editMsg)
		return err
	}

	// Extract minutes from callback data
	minutesStr := strings.TrimPrefix(data, "shutdown_")
	minutes, err := strconv.Atoi(minutesStr)
	if err != nil {
		return err
	}

	err = tb.shutdownMgr.ScheduleShutdown(minutes)
	if err != nil {
		msgText := fmt.Sprintf("❌ *Scheduling Failed*\n\nError: %v", err)
		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
		editMsg.ParseMode = "Markdown"
		editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
				{tgbotapi.NewInlineKeyboardButtonData("🏠 Main Menu", "main_menu")},
			},
		}
		_, err = tb.bot.Send(editMsg)
		return err
	}

	scheduledTime := tb.shutdownMgr.GetScheduledTime()
	msgText := fmt.Sprintf("⏰ *Shutdown Scheduled*\n\nThe computer will shutdown in %d minutes (%s).", minutes, scheduledTime.Format("15:04"))

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("❌ Cancel Shutdown", "cancel_shutdown")},
			{tgbotapi.NewInlineKeyboardButtonData("🏠 Main Menu", "main_menu")},
		},
	}

	_, err = tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) cancelShutdown(chatID int64, messageID int) error {
	err := tb.shutdownMgr.CancelShutdown()
	if err != nil {
		msgText := fmt.Sprintf("❌ *Cancel Failed*\n\nError: %v", err)
		editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
		editMsg.ParseMode = "Markdown"
		editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
				{tgbotapi.NewInlineKeyboardButtonData("🏠 Main Menu", "main_menu")},
			},
		}
		_, err = tb.bot.Send(editMsg)
		return err
	}

	msgText := "❌ *Shutdown Cancelled*\n\nThe scheduled shutdown has been cancelled."

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, msgText)
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("🏠 Main Menu", "main_menu")},
		},
	}

	_, err = tb.bot.Send(editMsg)
	return err
}

func (tb *TelegramBot) NotifySessionExpired(username string) {
	// Notify all authorized users about expired session
	for _, userID := range tb.config.AuthorizedUserIDs {
		msg := tgbotapi.NewMessage(userID, fmt.Sprintf("⏰ *Session Expired*\n\nUser %s's session has expired and has been locked.", username))
		msg.ParseMode = "Markdown"
		tb.bot.Send(msg)
	}
}
