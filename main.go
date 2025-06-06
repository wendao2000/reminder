package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
	"github.com/robfig/cron/v3"
)

type Reminder struct {
	ID        int
	ChannelID string
	UserID    string
	Message   string
	DueTime   time.Time
	CronExpr  sql.NullString
}

var (
	db            *sql.DB
	reminders     map[int]*time.Timer
	cronScheduler *cron.Cron
	cronEntries   sync.Map
	snoozeEntries sync.Map
)

var (
	parser = cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
)

func init() {
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		log.Fatal(err)
	}
	time.Local = loc
}

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Println("No .env file found, using system environment variables")
	}

	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		log.Fatal("DISCORD_TOKEN environment variable is not set")
	}

	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatal("Error creating Discord session:", err)
	}

	db, err = sql.Open("sqlite3", "./reminders.db")
	if err != nil {
		log.Fatal("Error opening database:", err)
	}
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS reminders (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        channel_id TEXT,
        user_id TEXT,
        message TEXT,
        due_time DATETIME,
        cron_expr TEXT
    )`)
	if err != nil {
		log.Fatal("Error creating table:", err)
	}

	reminders = make(map[int]*time.Timer)
	cronScheduler = cron.New(cron.WithSeconds())
	cronEntries = sync.Map{}

	dg.AddHandler(messageCreate)
	dg.AddHandler(interactionCreate)

	err = dg.Open()
	if err != nil {
		log.Fatal("Error opening connection:", err)
	}

	scheduleAllReminders(dg)
	cronScheduler.Start()

	fmt.Println("Bot is running. Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	cronScheduler.Stop()
	dg.Close()
}

func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == s.State.User.ID {
		return
	}

	parts := strings.Fields(m.Content)
	if len(parts) == 0 {
		return
	}

	switch parts[0] {
	case "!remind":
		handleRemindCommand(s, m, parts)
	case "!recurring":
		handleRecurringCommand(s, m, parts)
	case "!list":
		listReminders(s, m)
	case "!delete":
		handleDeleteCommand(s, m, parts)
	}
}

func handleRemindCommand(s *discordgo.Session, m *discordgo.MessageCreate, parts []string) {
	if len(parts) < 3 {
		s.ChannelMessageSend(m.ChannelID, "Usage: !remind <duration/time> <message> or !remind `<time>` <message>")
		return
	}

	var timeStr string
	var message string

	// Check if the time is enclosed in backticks
	if strings.HasPrefix(parts[1], "`") {
		args := parseBacktickArgs(strings.Join(parts[1:], " "))
		if len(args) < 2 {
			s.ChannelMessageSend(m.ChannelID, "Invalid command format. Please provide both time and message.")
			return
		}
		timeStr = args[0]
		message = strings.Join(args[1:], " ")
	} else {
		timeStr = parts[1]
		message = strings.Join(parts[2:], " ")
	}

	now := time.Now()
	var dueTime time.Time

	// First, try to parse as duration
	duration, err := parseDuration(timeStr)
	if err == nil {
		dueTime = now.Add(duration)
	} else {
		// If not a duration, try to parse as a specific time
		dueTime, err = parseFlexibleTime(timeStr)
		if err != nil {
			s.ChannelMessageSend(m.ChannelID, "Invalid time format. Use a duration (e.g., 5m, 2h, 1d) or a specific time (e.g., 2023-05-20T15:04:05).")
			return
		}
	}

	// Check if the due time is in the future
	if dueTime.Before(now) {
		s.ChannelMessageSend(m.ChannelID, "Error: Reminder time must be in the future.")
		return
	}

	reminder := Reminder{
		ChannelID: m.ChannelID,
		UserID:    m.Author.ID,
		Message:   message,
		DueTime:   dueTime,
	}

	id, err := saveReminder(reminder)
	if err != nil {
		s.ChannelMessageSend(m.ChannelID, "Error setting reminder: "+err.Error())
		return
	}

	scheduleReminder(s, id, reminder)

	s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Reminder set for <t:%d:F>, <t:%d:R> (ID: %d)", dueTime.Unix(), dueTime.Unix(), id))
}

func parseFlexibleTime(timeStr string) (time.Time, error) {
	// First, try to parse as AM/PM format
	if t, err := parseAMPM(timeStr); err == nil {
		return t, nil
	}

	// If AM/PM parsing fails, try other formats
	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04",
		"2006-01-02",
		"15:04:05",
		"15:04",
	}

	for _, format := range formats {
		if t, err := time.ParseInLocation(format, timeStr, time.Local); err == nil {
			// If only time is provided (not date), set it to today or tomorrow
			if len(timeStr) <= 8 { // Assuming time formats like "15:04:05" or "15:04"
				now := time.Now()
				t = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.Local)
				if t.Before(now) {
					t = t.AddDate(0, 0, 1) // Set to tomorrow if the time today has already passed
				}
			}
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("unable to parse time: %s", timeStr)
}

func parseAMPM(timeStr string) (time.Time, error) {
	re := regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?(?::(\d{2}))?\s*(am|pm)$`)
	matches := re.FindStringSubmatch(strings.ToLower(timeStr))

	if matches == nil {
		return time.Time{}, fmt.Errorf("invalid AM/PM time format")
	}

	hour, _ := strconv.Atoi(matches[1])
	minute, _ := strconv.Atoi(matches[2])
	second, _ := strconv.Atoi(matches[3])

	if matches[4] == "pm" && hour < 12 {
		hour += 12
	} else if matches[4] == "am" && hour == 12 {
		hour = 0
	}

	now := time.Now()
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, second, 0, time.Local)

	if t.Before(now) {
		t = t.AddDate(0, 0, 1) // Set to tomorrow if the time today has already passed
	}

	return t, nil
}

func handleRecurringCommand(s *discordgo.Session, m *discordgo.MessageCreate, parts []string) {
	fullCommand := strings.Join(parts[1:], " ")

	args := parseBacktickArgs(fullCommand)

	if len(args) < 2 {
		s.ChannelMessageSend(m.ChannelID, "Usage: !recurring `seconds minutes hours day_of_month month day_of_week` <message>")
		return
	}

	cronExpr := args[0]
	message := strings.Join(args[1:], " ")

	_, err := parser.Parse(cronExpr)
	if err != nil {
		s.ChannelMessageSend(m.ChannelID, "Invalid cron expression. Please check your syntax.")
		return
	}

	reminder := Reminder{
		ChannelID: m.ChannelID,
		UserID:    m.Author.ID,
		Message:   message,
		CronExpr: sql.NullString{
			Valid:  true,
			String: cronExpr,
		},
	}

	id, err := saveReminder(reminder)
	if err != nil {
		s.ChannelMessageSend(m.ChannelID, "Error setting recurring reminder: "+err.Error())
		return
	}

	scheduleRecurringReminder(s, id, reminder)

	s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Recurring reminder set with ID: %d", id))
}

func parseDuration(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}

	var valueStr, unit string
	for i, r := range s {
		if r < '0' || r > '9' {
			valueStr = s[:i]
			unit = s[i:]
			break
		}
	}

	if valueStr == "" || unit == "" {
		return 0, fmt.Errorf("invalid duration format")
	}

	value, err := strconv.Atoi(valueStr)
	if err != nil {
		return 0, err
	}

	switch strings.ToLower(unit) {
	case "m", "min", "mins", "minute", "minutes":
		return time.Duration(value) * time.Minute, nil
	case "h", "hr", "hrs", "hour", "hours":
		return time.Duration(value) * time.Hour, nil
	case "d", "day", "days":
		return time.Duration(value) * 24 * time.Hour, nil
	case "w", "wk", "wks", "week", "weeks":
		return time.Duration(value) * 7 * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("unknown time unit: %s", unit)
	}
}

func parseBacktickArgs(s string) []string {
	var args []string
	var currentArg strings.Builder
	inBackticks := false

	for _, r := range s {
		switch {
		case r == '`':
			if inBackticks {
				args = append(args, currentArg.String())
				currentArg.Reset()
			}
			inBackticks = !inBackticks
		case !inBackticks && unicode.IsSpace(r):
			if currentArg.Len() > 0 {
				args = append(args, currentArg.String())
				currentArg.Reset()
			}
		default:
			currentArg.WriteRune(r)
		}
	}

	if currentArg.Len() > 0 {
		args = append(args, currentArg.String())
	}

	return args
}

func scheduleReminder(s *discordgo.Session, id int, r Reminder) {
	duration := time.Until(r.DueTime)
	timer := time.AfterFunc(duration, func() {
		msg := &discordgo.MessageSend{
			Content: fmt.Sprintf("<@%s> Reminder: %s", r.UserID, r.Message),
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.SelectMenu{
							CustomID:    fmt.Sprintf("snooze:%d", id),
							Placeholder: "Snooze for...",
							Options: []discordgo.SelectMenuOption{
								{Label: "5 minutes", Value: "5m"},
								{Label: "10 minutes", Value: "10m"},
								{Label: "15 minutes", Value: "15m"},
								{Label: "30 minutes", Value: "30m"},
								{Label: "60 minutes", Value: "60m"},
							},
						},
					},
				},
			},
		}
		s.ChannelMessageSendComplex(r.ChannelID, msg)

		key := fmt.Sprintf("snooze:%d", id)
		snoozeEntries.Store(key, r)
		time.AfterFunc(5*time.Minute, func() { snoozeEntries.Delete(key) })
		deleteReminder(id)
	})
	reminders[id] = timer
}

func scheduleRecurringReminder(s *discordgo.Session, id int, r Reminder) {
	schedule, err := parser.Parse(r.CronExpr.String)
	if err != nil {
		log.Printf("Error parsing cron expression: %v", err)
		return
	}

	entryID := cronScheduler.Schedule(schedule, cron.FuncJob(func() {
		s.ChannelMessageSend(r.ChannelID, fmt.Sprintf("<@%s> Recurring Reminder (ID: %d): %s", r.UserID, id, r.Message))
	}))

	cronEntries.Store(id, entryID)
}

func scheduleAllReminders(s *discordgo.Session) {
	rows, err := db.Query("SELECT id, channel_id, user_id, message, due_time, cron_expr FROM reminders")
	if err != nil {
		log.Printf("Error fetching reminders: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var r Reminder
		var dueTimeStr sql.NullString
		err := rows.Scan(&r.ID, &r.ChannelID, &r.UserID, &r.Message, &dueTimeStr, &r.CronExpr)
		if err != nil {
			log.Printf("Error scanning reminder: %v", err)
			continue
		}

		if r.CronExpr.Valid && r.CronExpr.String != "" {
			scheduleRecurringReminder(s, r.ID, r)
		} else if dueTimeStr.Valid {
			r.DueTime, err = time.Parse(time.RFC3339, dueTimeStr.String)
			if err != nil {
				log.Printf("Error parsing due time: %v", err)
				continue
			}
			if time.Now().Before(r.DueTime) {
				scheduleReminder(s, r.ID, r)
			} else {
				deleteReminder(r.ID)
			}
		}
	}
}

func saveReminder(r Reminder) (int, error) {
	var result sql.Result
	var err error

	if r.CronExpr.Valid && r.CronExpr.String != "" {
		result, err = db.Exec("INSERT INTO reminders (channel_id, user_id, message, cron_expr) VALUES (?, ?, ?, ?)",
			r.ChannelID, r.UserID, r.Message, r.CronExpr)
	} else {
		result, err = db.Exec("INSERT INTO reminders (channel_id, user_id, message, due_time) VALUES (?, ?, ?, ?)",
			r.ChannelID, r.UserID, r.Message, r.DueTime.Format(time.RFC3339))
	}

	if err != nil {
		return 0, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}

	return int(id), nil
}

func deleteReminder(id int) error {
	_, err := db.Exec("DELETE FROM reminders WHERE id = ?", id)
	if err != nil {
		return err
	}

	if timer, exists := reminders[id]; exists {
		timer.Stop()
		delete(reminders, id)
	}

	if entryIDInterface, ok := cronEntries.Load(id); ok {
		entryID, ok := entryIDInterface.(cron.EntryID)
		if ok {
			cronScheduler.Remove(entryID)
		}
		cronEntries.Delete(id)
	}

	return nil
}

func handleDeleteCommand(s *discordgo.Session, m *discordgo.MessageCreate, parts []string) {
	if len(parts) != 2 {
		s.ChannelMessageSend(m.ChannelID, "Usage: !delete <id>")
		return
	}

	id, err := strconv.Atoi(parts[1])
	if err != nil {
		s.ChannelMessageSend(m.ChannelID, "Invalid reminder ID")
		return
	}

	var userID string
	err = db.QueryRow("SELECT user_id FROM reminders WHERE id = ?", id).Scan(&userID)
	if err != nil {
		if err == sql.ErrNoRows {
			s.ChannelMessageSend(m.ChannelID, "Reminder not found")
		} else {
			s.ChannelMessageSend(m.ChannelID, "Error checking reminder ownership: "+err.Error())
		}
		return
	}

	if userID != m.Author.ID {
		s.ChannelMessageSend(m.ChannelID, "You can only delete your own reminders")
		return
	}

	err = deleteReminder(id)
	if err != nil {
		s.ChannelMessageSend(m.ChannelID, "Error deleting reminder: "+err.Error())
		return
	}

	s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Reminder %d deleted", id))
}

func listReminders(s *discordgo.Session, m *discordgo.MessageCreate) {
	rows, err := db.Query("SELECT id, message, due_time, cron_expr FROM reminders WHERE user_id = ?", m.Author.ID)
	if err != nil {
		s.ChannelMessageSend(m.ChannelID, "Error fetching reminders: "+err.Error())
		return
	}
	defer rows.Close()

	var reminders strings.Builder
	reminders.WriteString("Your reminders:\n")

	for rows.Next() {
		var id int
		var message string
		var dueTimeNullStr, cronExpr sql.NullString
		err := rows.Scan(&id, &message, &dueTimeNullStr, &cronExpr)
		if err != nil {
			log.Printf("Error scanning reminder: %v", err)
			continue
		}

		if cronExpr.Valid && cronExpr.String != "" {
			schedule, _ := parser.Parse(cronExpr.String)
			now := time.Now()
			next := schedule.Next(now)
			reminders.WriteString(fmt.Sprintf("%d: %s (recurring: %s, next: <t:%d:F>, <t:%d:R>)\n", id, message, cronExpr.String, next.Unix(), next.Unix()))
		} else if dueTimeNullStr.Valid {
			dueTime, err := time.Parse(time.RFC3339, dueTimeNullStr.String)
			if err != nil {
				log.Printf("Error parsing due time: %v", err)
				continue
			}
			reminders.WriteString(fmt.Sprintf("%d: %s (due <t:%d:F>, <t:%d:R>)\n", id, message, dueTime.Unix(), dueTime.Unix()))
		}
	}

	if reminders.Len() == 0 {
		s.ChannelMessageSend(m.ChannelID, "You have no reminders set")
	} else {
		s.ChannelMessageSend(m.ChannelID, reminders.String())
	}
}

func interactionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionMessageComponent {
		return
	}

	data := i.MessageComponentData()
	switch {
	case strings.HasPrefix(data.CustomID, "snooze:"):
		handleSnoozeInteraction(s, i, &data)
	}
}

func handleSnoozeInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, data *discordgo.MessageComponentInteractionData) {
	rInterface, ok := snoozeEntries.Load(data.CustomID)
	if !ok {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "Snooze expired. Please create a new reminder",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	r := rInterface.(Reminder)
	dur, _ := time.ParseDuration(data.Values[0])
	r.DueTime = time.Now().Add(dur)

	id, err := saveReminder(r)
	if err == nil {
		scheduleReminder(s, id, r)
	}

	snoozeEntries.Delete(data.CustomID)

	if err != nil {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "Error scheduling snoozed reminder: " + err.Error(),
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	} else {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: fmt.Sprintf("Snoozed for %s", data.Values[0]),
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}
}
