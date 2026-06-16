package bot

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/sglre6355/zensur/internal/censor"
)

// attachmentDownloadTimeout bounds how long the bot waits when fetching an
// image attachment for LLM inspection.
const attachmentDownloadTimeout = 20 * time.Second

// webhookName is the marker used to find / create the bot's channel webhook
// for the "replace" action. Re-using a single named webhook avoids creating a
// new one on every restart.
const webhookName = "zensur"

// Bot is a Discord client that censors messages according to a compiled
// ruleset.
type Bot struct {
	cfg        *Config
	ruleset    *censor.Ruleset
	llm        *censor.LLMFilter // optional semantic filter; nil when disabled
	session    *discordgo.Session
	httpClient *http.Client // fetches image attachments for the LLM filter

	webhookMu      sync.Mutex
	webhooks       map[string]*discordgo.Webhook // channelID → webhook
	removeHandlers []func()

	// metaMu guards the last-known-good metadata caches used to revert
	// rule-violating guild and channel updates.
	metaMu      sync.RWMutex
	guildMeta   map[string]guildMeta   // guildID → metadata
	channelMeta map[string]channelMeta // channelID → metadata
}

func NewBot(cfg *Config) *Bot {
	return &Bot{
		cfg:         cfg,
		webhooks:    make(map[string]*discordgo.Webhook),
		guildMeta:   make(map[string]guildMeta),
		channelMeta: make(map[string]channelMeta),
		httpClient:  &http.Client{Timeout: attachmentDownloadTimeout},
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

	if lc := b.cfg.Censor.LLM; lc != nil && lc.Enabled {
		f, err := censor.NewLLMFilter(lc)
		if err != nil {
			return fmt.Errorf("init llm filter: %w", err)
		}
		b.llm = f
		slog.Info("llm filter enabled",
			"provider", f.Provider(), "model", f.Model(), "action", f.Action().String(),
			"images", f.ImagesEnabled())
	}

	s, err := discordgo.New("Bot " + b.cfg.Token)
	if err != nil {
		return fmt.Errorf("create discord session: %w", err)
	}
	// MessageContent is a privileged intent — it must also be enabled for the
	// application in the Discord Developer Portal. IntentsGuilds delivers the
	// guild/channel create and update events the metadata guard relies on.
	s.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentMessageContent
	b.session = s

	b.removeHandlers = append(b.removeHandlers,
		s.AddHandler(b.onMessageCreate),
		s.AddHandler(b.onMessageUpdate),
		s.AddHandler(b.onInteractionCreate),
		s.AddHandler(b.onGuildCreate),
		s.AddHandler(b.onChannelCreate),
		s.AddHandler(b.onGuildUpdate),
		s.AddHandler(b.onChannelUpdate),
	)

	if err := s.Open(); err != nil {
		return fmt.Errorf("open discord session: %w", err)
	}
	slog.Info("discord session opened")

	if err := b.registerCommands(s); err != nil {
		return fmt.Errorf("register commands: %w", err)
	}
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
	if msg == nil {
		return
	}
	images := b.filterableImages(msg)
	if msg.Content == "" && len(images) == 0 {
		return
	}

	var hits []censor.Hit
	if msg.Content != "" {
		hits = b.ruleset.Match(msg.Content)
	}

	// Start from the most disruptive pattern-rule action (ActionLog when no
	// rule matched) and escalate if the semantic filter also flags the message.
	flagged := len(hits) > 0
	action := censor.MaxAction(hits)

	llmFlagged := false
	var llmReason string
	if b.llm != nil && msg.Content != "" {
		verdict, err := b.llm.Evaluate(context.Background(), msg.Content)
		switch {
		case err != nil:
			slog.Error("llm filter failed",
				"channel_id", msg.ChannelID, "message_id", msg.ID, "error", err)
		case verdict.Flagged:
			flagged = true
			llmFlagged = true
			llmReason = verdict.Reason
			action = censor.MoreSevere(action, b.llm.Action())
		}
	}

	if reason, ok := b.evaluateImages(msg, images); ok {
		flagged = true
		llmFlagged = true
		if llmReason == "" {
			llmReason = reason
		}
		action = censor.MoreSevere(action, b.llm.ImageAction())
	}

	if !flagged {
		return
	}

	slog.Info("censor hit",
		"action", action.String(),
		"rules", uniqueRuleIDs(hits),
		"llm", llmFlagged,
		"llm_reason", llmReason,
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
		b.sendNotice(s, msg, hits, llmFlagged)
	case censor.ActionReplace:
		// Replace only ever comes from a pattern rule (the LLM action is limited
		// to log/delete/warn), so hits are guaranteed non-empty here.
		censored := b.ruleset.Censor(msg.Content, hits)
		b.deleteMessage(s, msg)
		if err := b.repostViaWebhook(s, msg, censored); err != nil {
			slog.Error("webhook repost failed; falling back to notice",
				"channel_id", msg.ChannelID, "error", err)
			b.sendNotice(s, msg, hits, llmFlagged)
		}
	}
}

// filterableImages selects the image attachments eligible for LLM inspection:
// content-type image/*, within the size limit, capped at the configured count.
// Returns nil when image filtering is disabled.
func (b *Bot) filterableImages(msg *discordgo.Message) []*discordgo.MessageAttachment {
	if b.llm == nil || !b.llm.ImagesEnabled() || len(msg.Attachments) == 0 {
		return nil
	}
	maxBytes := b.llm.ImageMaxBytes()
	limit := b.llm.ImageMaxCount()
	var out []*discordgo.MessageAttachment
	for _, att := range msg.Attachments {
		if att == nil || !isImageAttachment(att) {
			continue
		}
		if maxBytes > 0 && att.Size > 0 && int64(att.Size) > maxBytes {
			slog.Debug("skipping oversize image attachment",
				"channel_id", msg.ChannelID, "message_id", msg.ID,
				"filename", att.Filename, "size", att.Size, "max_bytes", maxBytes)
			continue
		}
		out = append(out, att)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// evaluateImages downloads and checks each candidate image, stopping at the
// first one the model flags. It returns the flag reason and true on a hit. A
// nil llm or empty list yields ("", false).
func (b *Bot) evaluateImages(msg *discordgo.Message, images []*discordgo.MessageAttachment) (string, bool) {
	if b.llm == nil || len(images) == 0 {
		return "", false
	}
	for _, att := range images {
		data, mime, err := b.downloadAttachment(att)
		if err != nil {
			slog.Error("image download failed",
				"channel_id", msg.ChannelID, "message_id", msg.ID,
				"filename", att.Filename, "error", err)
			continue
		}
		verdict, err := b.llm.EvaluateImage(context.Background(), mime, data, msg.Content)
		if err != nil {
			slog.Error("llm image filter failed",
				"channel_id", msg.ChannelID, "message_id", msg.ID,
				"filename", att.Filename, "error", err)
			continue
		}
		if verdict.Flagged {
			return verdict.Reason, true
		}
	}
	return "", false
}

// downloadAttachment fetches an attachment's bytes (bounded by the size limit)
// and resolves its MIME type, preferring the content-type Discord reports and
// falling back to the HTTP response header.
func (b *Bot) downloadAttachment(att *discordgo.MessageAttachment) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), attachmentDownloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, att.URL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("download status %d", resp.StatusCode)
	}

	// Cap the read at the configured size limit (+1 to detect overflow).
	limit := b.llm.ImageMaxBytes()
	reader := io.Reader(resp.Body)
	if limit > 0 {
		reader = io.LimitReader(resp.Body, limit+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, "", err
	}
	if limit > 0 && int64(len(data)) > limit {
		return nil, "", fmt.Errorf("image exceeds %d bytes", limit)
	}

	mime := att.ContentType
	if mime == "" {
		mime = resp.Header.Get("Content-Type")
	}
	if i := strings.IndexByte(mime, ';'); i >= 0 { // strip "; charset=..."
		mime = mime[:i]
	}
	mime = strings.TrimSpace(mime)
	if mime == "" {
		return nil, "", fmt.Errorf("missing content type for %s", att.Filename)
	}
	return data, mime, nil
}

// isImageAttachment reports whether an attachment is an image, by its reported
// content type or, failing that, a common image file extension.
func isImageAttachment(att *discordgo.MessageAttachment) bool {
	if strings.HasPrefix(strings.ToLower(att.ContentType), "image/") {
		return true
	}
	if att.ContentType != "" {
		return false
	}
	name := strings.ToLower(att.Filename)
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".gif", ".webp"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

func (b *Bot) deleteMessage(s *discordgo.Session, msg *discordgo.Message) {
	if err := s.ChannelMessageDelete(msg.ChannelID, msg.ID); err != nil {
		slog.Error("delete message failed",
			"channel_id", msg.ChannelID, "message_id", msg.ID, "error", err)
	}
}

func (b *Bot) sendNotice(s *discordgo.Session, msg *discordgo.Message, hits []censor.Hit, llmFlagged bool) {
	notice := b.noticeText(hits, llmFlagged)
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

func (b *Bot) noticeText(hits []censor.Hit, llmFlagged bool) string {
	// Prefer a matching rule's own notice, then the LLM filter's notice (when it
	// was the trigger), then the global notice, then a built-in default.
	for _, h := range hits {
		if r := b.ruleset.RuleByID(h.RuleID); r != nil && r.Notice != "" {
			return r.Notice
		}
	}
	if llmFlagged && b.llm != nil && b.llm.Notice() != "" {
		return b.llm.Notice()
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
