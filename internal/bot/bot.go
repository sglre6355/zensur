package bot

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/sglre6355/zensur/internal/censor"
)

// webhookName is the marker used to find / create the bot's channel webhook
// for the "replace" action. Re-using a single named webhook avoids creating a
// new one on every restart.
const webhookName = "zensur"

// Bot is a Discord client that censors messages according to a compiled
// ruleset.
type Bot struct {
	cfg     *Config
	ruleset *censor.Ruleset
	session *discordgo.Session

	webhookMu      sync.Mutex
	webhooks       map[string]*discordgo.Webhook // channelID → webhook
	removeHandlers []func()
}

func NewBot(cfg *Config) *Bot {
	return &Bot{
		cfg:      cfg,
		webhooks: make(map[string]*discordgo.Webhook),
	}
}

// Start compiles the ruleset, opens the Discord session, and attaches
// message-event handlers.
func (b *Bot) Start() error {
	rs, err := censor.Compile(b.cfg.Censor)
	if err != nil {
		return fmt.Errorf("compile ruleset: %w", err)
	}
	b.ruleset = rs
	slog.Info("ruleset compiled", "rules", len(rs.Rules))

	s, err := discordgo.New("Bot " + b.cfg.Token)
	if err != nil {
		return fmt.Errorf("create discord session: %w", err)
	}
	// MessageContent is a privileged intent — it must also be enabled for the
	// application in the Discord Developer Portal.
	s.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentMessageContent
	b.session = s

	b.removeHandlers = append(b.removeHandlers,
		s.AddHandler(b.onMessageCreate),
		s.AddHandler(b.onMessageUpdate),
	)

	if err := s.Open(); err != nil {
		return fmt.Errorf("open discord session: %w", err)
	}
	slog.Info("discord session opened")
	return nil
}

func (b *Bot) Stop() error {
	for _, rm := range b.removeHandlers {
		rm()
	}
	b.removeHandlers = nil
	if b.session != nil {
		return b.session.Close()
	}
	return nil
}

func (b *Bot) onMessageCreate(s *discordgo.Session, e *discordgo.MessageCreate) {
	if e.Author == nil || e.Author.Bot {
		return
	}
	b.process(s, e.Message)
}

func (b *Bot) onMessageUpdate(s *discordgo.Session, e *discordgo.MessageUpdate) {
	if e.Author == nil || e.Author.Bot {
		return
	}
	b.process(s, e.Message)
}

func (b *Bot) process(s *discordgo.Session, msg *discordgo.Message) {
	if msg == nil || msg.Content == "" {
		return
	}
	hits := b.ruleset.Match(msg.Content)
	if len(hits) == 0 {
		return
	}

	action := censor.MaxAction(hits)
	slog.Info("censor hit",
		"action", action.String(),
		"rules", uniqueRuleIDs(hits),
		"user_id", msg.Author.ID,
		"guild_id", msg.GuildID,
		"channel_id", msg.ChannelID,
		"message_id", msg.ID,
	)

	switch action {
	case censor.ActionLog:
		// Logged above; nothing else to do.
	case censor.ActionDelete:
		b.deleteMessage(s, msg)
	case censor.ActionWarn:
		b.deleteMessage(s, msg)
		b.sendNotice(s, msg, hits)
	case censor.ActionReplace:
		censored := b.ruleset.Censor(msg.Content, hits)
		b.deleteMessage(s, msg)
		if err := b.repostViaWebhook(s, msg, censored); err != nil {
			slog.Error("webhook repost failed; falling back to notice",
				"channel_id", msg.ChannelID, "error", err)
			b.sendNotice(s, msg, hits)
		}
	}
}

func (b *Bot) deleteMessage(s *discordgo.Session, msg *discordgo.Message) {
	if err := s.ChannelMessageDelete(msg.ChannelID, msg.ID); err != nil {
		slog.Error("delete message failed",
			"channel_id", msg.ChannelID, "message_id", msg.ID, "error", err)
	}
}

func (b *Bot) sendNotice(s *discordgo.Session, msg *discordgo.Message, hits []censor.Hit) {
	notice := b.noticeText(hits)
	if notice == "" {
		return
	}
	sent, err := s.ChannelMessageSend(msg.ChannelID, notice)
	if err != nil {
		slog.Error("send notice failed",
			"channel_id", msg.ChannelID, "error", err)
		return
	}
	if b.ruleset.NoticeAutoDeleteSeconds > 0 {
		go b.autoDelete(s, sent.ChannelID, sent.ID, b.ruleset.NoticeAutoDeleteSeconds)
	}
}

func (b *Bot) noticeText(hits []censor.Hit) string {
	for _, h := range hits {
		if r := b.ruleset.RuleByID(h.RuleID); r != nil && r.Notice != "" {
			return r.Notice
		}
	}
	if b.ruleset.Notice != "" {
		return b.ruleset.Notice
	}
	return "Your message was removed."
}

func (b *Bot) autoDelete(s *discordgo.Session, channelID, messageID string, seconds int) {
	time.Sleep(time.Duration(seconds) * time.Second)
	if err := s.ChannelMessageDelete(channelID, messageID); err != nil {
		slog.Debug("auto-delete notice failed",
			"channel_id", channelID, "message_id", messageID, "error", err)
	}
}

// repostViaWebhook deletes-and-reposts a censored copy of the message under
// the original author's name and avatar. Requires Manage Webhooks in the
// channel.
func (b *Bot) repostViaWebhook(s *discordgo.Session, original *discordgo.Message, content string) error {
	hook, err := b.webhookFor(s, original.ChannelID)
	if err != nil {
		return err
	}

	username := original.Author.Username
	if original.Member != nil && original.Member.Nick != "" {
		username = original.Member.Nick
	}
	avatar := original.Author.AvatarURL("")

	_, err = s.WebhookExecute(hook.ID, hook.Token, false, &discordgo.WebhookParams{
		Content:   content,
		Username:  username,
		AvatarURL: avatar,
	})
	return err
}

func (b *Bot) webhookFor(s *discordgo.Session, channelID string) (*discordgo.Webhook, error) {
	b.webhookMu.Lock()
	defer b.webhookMu.Unlock()

	if w, ok := b.webhooks[channelID]; ok {
		return w, nil
	}
	hooks, err := s.ChannelWebhooks(channelID)
	if err != nil {
		return nil, fmt.Errorf("list webhooks: %w", err)
	}
	for _, h := range hooks {
		if h.Name == webhookName && h.Token != "" {
			b.webhooks[channelID] = h
			return h, nil
		}
	}
	created, err := s.WebhookCreate(channelID, webhookName, "")
	if err != nil {
		return nil, fmt.Errorf("create webhook: %w", err)
	}
	b.webhooks[channelID] = created
	return created, nil
}

func uniqueRuleIDs(hits []censor.Hit) []string {
	seen := make(map[string]struct{}, len(hits))
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		if _, ok := seen[h.RuleID]; ok {
			continue
		}
		seen[h.RuleID] = struct{}{}
		out = append(out, h.RuleID)
	}
	return out
}
