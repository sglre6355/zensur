package bot

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/bwmarrin/discordgo"
)

// purgeCommandName is the slash command that bulk-deletes the last N messages
// in the channel it is invoked from. Its reply is ephemeral, so only the
// invoking moderator sees the result.
const purgeCommandName = "purge"

// purgeMaxMessages is the most messages a single /purge invocation may remove.
// Discord's bulk-delete endpoint accepts at most 100 IDs per call.
const purgeMaxMessages = 100

// bulkDeleteMaxAge is the cutoff beyond which Discord refuses to bulk-delete a
// message; older messages must be deleted one at a time.
const bulkDeleteMaxAge = 14 * 24 * time.Hour

// statusCommandName is the slash command that sets the bot's presence
// (activity line and online status). Like /purge it replies ephemerally.
const statusCommandName = "status"

// statusActivityTypes maps the choice values exposed in the /status command to
// discordgo activity types. The keys are also the choice values sent back to
// Discord, so they must stay in sync with the Choices list below.
var statusActivityTypes = map[string]discordgo.ActivityType{
	"playing":   discordgo.ActivityTypeGame,
	"watching":  discordgo.ActivityTypeWatching,
	"listening": discordgo.ActivityTypeListening,
	"competing": discordgo.ActivityTypeCompeting,
	"custom":    discordgo.ActivityTypeCustom,
}

// statusPresences maps the choice values for the optional "presence" option to
// the status strings Discord expects.
var statusPresences = map[string]string{
	"online":    "online",
	"idle":      "idle",
	"dnd":       "dnd",
	"invisible": "invisible",
}

// statusCommand sets an arbitrary presence for the bot. It is gated to
// administrators since it changes the bot's global, guild-independent identity.
var statusCommand = &discordgo.ApplicationCommand{
	Name:                     statusCommandName,
	Description:              "Set the bot's status (presence)",
	DefaultMemberPermissions: ptr(int64(discordgo.PermissionAdministrator)),
	DMPermission:             ptr(false),
	Options: []*discordgo.ApplicationCommandOption{
		{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        "text",
			Description: "The status text (leave empty to clear the presence)",
			Required:    false,
		},
		{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        "type",
			Description: "Activity type (defaults to Playing)",
			Required:    false,
			Choices: []*discordgo.ApplicationCommandOptionChoice{
				{Name: "Playing", Value: "playing"},
				{Name: "Watching", Value: "watching"},
				{Name: "Listening", Value: "listening"},
				{Name: "Competing", Value: "competing"},
				{Name: "Custom", Value: "custom"},
			},
		},
		{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        "presence",
			Description: "Online status (defaults to online)",
			Required:    false,
			Choices: []*discordgo.ApplicationCommandOptionChoice{
				{Name: "Online", Value: "online"},
				{Name: "Idle", Value: "idle"},
				{Name: "Do Not Disturb", Value: "dnd"},
				{Name: "Invisible", Value: "invisible"},
			},
		},
	},
}

// purgeCommand is the application command definition registered with Discord.
//
// DefaultMemberPermissions restricts visibility/use to members with Manage
// Messages, and DMPermission disables it in DMs (there is nothing to moderate
// there, and bulk delete is a guild-only endpoint).
var purgeCommand = &discordgo.ApplicationCommand{
	Name:                     purgeCommandName,
	Description:              "Delete the last N messages in this channel",
	DefaultMemberPermissions: ptr(int64(discordgo.PermissionManageMessages)),
	DMPermission:             ptr(false),
	Options: []*discordgo.ApplicationCommandOption{
		{
			Type:        discordgo.ApplicationCommandOptionInteger,
			Name:        "count",
			Description: fmt.Sprintf("How many messages to delete (1-%d)", purgeMaxMessages),
			Required:    true,
			MinValue:    ptr(float64(1)),
			MaxValue:    purgeMaxMessages,
		},
	},
}

// registerCommands creates (or updates) the bot's slash commands. It must be
// called after the session is open so that the application ID is known.
func (b *Bot) registerCommands(s *discordgo.Session) error {
	appID := s.State.User.ID
	for _, cmd := range []*discordgo.ApplicationCommand{purgeCommand, statusCommand} {
		if _, err := s.ApplicationCommandCreate(appID, "", cmd); err != nil {
			return fmt.Errorf("register %q command: %w", cmd.Name, err)
		}
		slog.Info("slash command registered", "command", cmd.Name)
	}
	return nil
}

func (b *Bot) onInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	switch i.ApplicationCommandData().Name {
	case purgeCommandName:
		b.handlePurge(s, i)
	case statusCommandName:
		b.handleStatus(s, i)
	}
}

func (b *Bot) handlePurge(s *discordgo.Session, i *discordgo.InteractionCreate) {
	count := int(i.ApplicationCommandData().Options[0].IntValue())

	// Acknowledge immediately and privately; the work below may take longer
	// than Discord's 3-second response window when old messages need to be
	// deleted individually.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	}); err != nil {
		slog.Error("purge: defer response failed", "channel_id", i.ChannelID, "error", err)
		return
	}

	deleted, err := b.purgeMessages(s, i.ChannelID, count)
	if err != nil {
		slog.Error("purge failed",
			"channel_id", i.ChannelID, "user_id", interactionUserID(i),
			"requested", count, "deleted", deleted, "error", err)
		b.editInteraction(s, i, fmt.Sprintf("Deleted %d message(s), then hit an error: %v", deleted, err))
		return
	}

	slog.Info("purge",
		"channel_id", i.ChannelID, "user_id", interactionUserID(i),
		"requested", count, "deleted", deleted)
	b.editInteraction(s, i, fmt.Sprintf("Deleted %d message(s).", deleted))
}

func (b *Bot) handleStatus(s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := optionMap(i.ApplicationCommandData().Options)

	typeChoice := "playing"
	if opt, ok := opts["type"]; ok {
		typeChoice = opt.StringValue()
	}
	text := ""
	if opt, ok := opts["text"]; ok {
		text = opt.StringValue()
	}
	presenceChoice := "online"
	if opt, ok := opts["presence"]; ok {
		presenceChoice = opt.StringValue()
	}

	// An empty text clears the activity line; Discord shows the bot with only
	// its online status and no "Playing ..." text.
	var activities []*discordgo.Activity
	if text != "" {
		activityType := statusActivityTypes[typeChoice]
		activity := &discordgo.Activity{
			Name: text,
			Type: activityType,
		}
		// A custom status carries its text in State; Discord still requires a
		// non-empty Name, so a placeholder is used (matching discordgo's own
		// UpdateCustomStatus helper).
		if activityType == discordgo.ActivityTypeCustom {
			activity.Name = "Custom Status"
			activity.State = text
		}
		activities = []*discordgo.Activity{activity}
	}

	if err := s.UpdateStatusComplex(discordgo.UpdateStatusData{
		Status:     statusPresences[presenceChoice],
		Activities: activities,
	}); err != nil {
		slog.Error("status update failed", "user_id", interactionUserID(i), "error", err)
		b.respondEphemeral(s, i, fmt.Sprintf("Failed to update status: %v", err))
		return
	}

	slog.Info("status updated",
		"user_id", interactionUserID(i), "type", typeChoice, "text", text, "presence", presenceChoice)
	if text == "" {
		b.respondEphemeral(s, i, "Status cleared.")
	} else {
		b.respondEphemeral(s, i, "Status updated.")
	}
}

// purgeMessages deletes up to count most-recent messages from the channel and
// returns how many were actually removed. Messages within the bulk-delete age
// window are removed in a single call; older ones are deleted individually.
func (b *Bot) purgeMessages(s *discordgo.Session, channelID string, count int) (int, error) {
	msgs, err := s.ChannelMessages(channelID, count, "", "", "")
	if err != nil {
		return 0, fmt.Errorf("fetch messages: %w", err)
	}

	cutoff := time.Now().Add(-bulkDeleteMaxAge)
	var recent, old []string
	for _, m := range msgs {
		if ts, err := discordgo.SnowflakeTimestamp(m.ID); err == nil && ts.Before(cutoff) {
			old = append(old, m.ID)
		} else {
			recent = append(recent, m.ID)
		}
	}

	deleted := 0
	switch {
	case len(recent) == 1:
		// Bulk delete requires at least 2 IDs; fall back to a single delete.
		if err := s.ChannelMessageDelete(channelID, recent[0]); err != nil {
			return deleted, fmt.Errorf("delete message: %w", err)
		}
		deleted++
	case len(recent) > 1:
		if err := s.ChannelMessagesBulkDelete(channelID, recent); err != nil {
			return deleted, fmt.Errorf("bulk delete: %w", err)
		}
		deleted += len(recent)
	}

	for _, id := range old {
		if err := s.ChannelMessageDelete(channelID, id); err != nil {
			return deleted, fmt.Errorf("delete old message: %w", err)
		}
		deleted++
	}
	return deleted, nil
}

func (b *Bot) editInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: &content,
	}); err != nil {
		slog.Error("purge: edit response failed", "channel_id", i.ChannelID, "error", err)
	}
}

// respondEphemeral sends an immediate, private reply to an interaction.
func (b *Bot) respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	}); err != nil {
		slog.Error("respond failed", "user_id", interactionUserID(i), "error", err)
	}
}

// optionMap indexes interaction options by name so handlers can read them
// without relying on positional order (which matters when some are optional).
func optionMap(opts []*discordgo.ApplicationCommandInteractionDataOption) map[string]*discordgo.ApplicationCommandInteractionDataOption {
	m := make(map[string]*discordgo.ApplicationCommandInteractionDataOption, len(opts))
	for _, opt := range opts {
		m[opt.Name] = opt
	}
	return m
}

// interactionUserID returns the invoking user's ID, whether the interaction
// arrived from a guild (Member) or a DM (User).
func interactionUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

func ptr[T any](v T) *T { return &v }
