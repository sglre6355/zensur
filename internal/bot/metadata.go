package bot

import (
	"log/slog"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/sglre6355/zensur/internal/censor"
)

// guildMeta and channelMeta hold the last-known-good text metadata for a guild
// or channel. An entry is seeded the first time the bot sees the entity
// (GuildCreate / ChannelCreate) and refreshed on every accepted update. When an
// update violates the ruleset, the offending field is reverted to the value
// stored here.
type guildMeta struct {
	name        string
	description string
}

type channelMeta struct {
	name  string
	topic string
}

func (b *Bot) onGuildCreate(s *discordgo.Session, e *discordgo.GuildCreate) {
	if e.Guild == nil {
		return
	}
	b.metaMu.Lock()
	b.guildMeta[e.ID] = guildMeta{name: e.Name, description: e.Description}
	for _, c := range e.Channels {
		if c != nil {
			b.channelMeta[c.ID] = channelMeta{name: c.Name, topic: c.Topic}
		}
	}
	b.metaMu.Unlock()
}

func (b *Bot) onChannelCreate(s *discordgo.Session, e *discordgo.ChannelCreate) {
	if e.Channel == nil || e.GuildID == "" {
		return
	}
	b.metaMu.Lock()
	b.channelMeta[e.ID] = channelMeta{name: e.Name, topic: e.Topic}
	b.metaMu.Unlock()
}

func (b *Bot) onGuildUpdate(s *discordgo.Session, e *discordgo.GuildUpdate) {
	if e.Guild == nil {
		return
	}
	b.metaMu.RLock()
	prev, seen := b.guildMeta[e.ID]
	b.metaMu.RUnlock()
	if !seen {
		// First sighting (e.g. the bot joined after this guild already existed):
		// record the baseline rather than enforcing against an unknown previous
		// value.
		b.metaMu.Lock()
		b.guildMeta[e.ID] = guildMeta{name: e.Name, description: e.Description}
		b.metaMu.Unlock()
		return
	}

	name, nameChanged := b.guardField("guild.name", e.ID, e.ID,
		metaField{name: "name", current: e.Name, previous: prev.name})
	desc, descChanged := b.guardField("guild.description", e.ID, e.ID,
		metaField{name: "description", current: e.Description, previous: prev.description})

	if nameChanged || descChanged {
		params := &discordgo.GuildParams{}
		if nameChanged {
			params.Name = name
		}
		if descChanged {
			// Note: GuildParams.Description is omitempty, so reverting to an empty
			// description cannot clear a non-empty one through this call.
			params.Description = desc
		}
		if _, err := s.GuildEdit(e.ID, params); err != nil {
			slog.Error("guild metadata revert failed", "guild_id", e.ID, "error", err)
			return
		}
	}

	b.metaMu.Lock()
	b.guildMeta[e.ID] = guildMeta{name: name, description: desc}
	b.metaMu.Unlock()
}

func (b *Bot) onChannelUpdate(s *discordgo.Session, e *discordgo.ChannelUpdate) {
	if e.Channel == nil || e.GuildID == "" {
		return
	}
	b.metaMu.RLock()
	prev, seen := b.channelMeta[e.ID]
	b.metaMu.RUnlock()
	if !seen {
		b.metaMu.Lock()
		b.channelMeta[e.ID] = channelMeta{name: e.Name, topic: e.Topic}
		b.metaMu.Unlock()
		return
	}

	name, nameChanged := b.guardField("channel.name", e.ID, e.GuildID,
		metaField{name: "name", current: e.Name, previous: prev.name})
	topic, topicChanged := b.guardField("channel.topic", e.ID, e.GuildID,
		metaField{name: "topic", current: e.Topic, previous: prev.topic})

	if nameChanged || topicChanged {
		data := &discordgo.ChannelEdit{}
		if nameChanged {
			data.Name = name
		}
		if topicChanged {
			// Note: ChannelEdit.Topic is omitempty, so reverting to an empty topic
			// cannot clear a non-empty one through this call.
			data.Topic = topic
		}
		if _, err := s.ChannelEdit(e.ID, data); err != nil {
			slog.Error("channel metadata revert failed", "channel_id", e.ID, "error", err)
			return
		}
	}

	b.metaMu.Lock()
	b.channelMeta[e.ID] = channelMeta{name: name, topic: topic}
	b.metaMu.Unlock()
}

// metaField is one named text field being guarded on a metadata update.
type metaField struct {
	name     string // field label for logging, e.g. "name", "topic"
	current  string // value carried by the update event
	previous string // last-known-good value from the cache
}

// guardField runs the ruleset against a changed metadata field and decides the
// value it should end up with. It returns the resolved value and whether that
// value differs from current (i.e. enforcement was applied). Every enforcement
// is logged. The action is interpreted the same way as for messages:
//
//	log              → allow the change to stand (log only)
//	replace          → keep the change but censor the offending spans in place
//	warn / delete    → revert the field to its last-known-good value
//
// Only fields actually changed by the update are enforced; pre-existing values
// the bot never saw change are left untouched.
func (b *Bot) guardField(scope, id, guildID string, f metaField) (string, bool) {
	if f.current == f.previous {
		return f.current, false // unchanged by this update; nothing to enforce
	}
	hits := b.ruleset.Match(f.current)
	if len(hits) == 0 {
		return f.current, false // changed but clean; accept the new value
	}

	action := censor.MaxAction(hits)
	resolved := f.current
	switch action {
	case censor.ActionLog:
		// Log only; allow the change to stand.
	case censor.ActionReplace:
		resolved = b.ruleset.Censor(f.current, hits)
		if strings.TrimSpace(resolved) == "" {
			// A fully-censored field would be an invalid name/topic; revert instead.
			resolved = f.previous
		}
	default: // ActionWarn, ActionDelete
		resolved = f.previous
	}

	reverted := resolved != f.current
	slog.Info("metadata guard hit",
		"scope", scope,
		"field", f.name,
		"action", action.String(),
		"rules", uniqueRuleIDs(hits),
		"guild_id", guildID,
		"id", id,
		"reverted", reverted,
	)
	return resolved, reverted
}
