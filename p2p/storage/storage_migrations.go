package storage

import (
	"context"
	"database/sql"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
)

func (s *DatabaseStore) migrate(ctx context.Context) error {
	m := sqlutil.NewMigrator(s.db)
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: integrated appservice tables v1",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`CREATE TABLE IF NOT EXISTS p2p_portal (
						id TEXT PRIMARY KEY NOT NULL,
						initialized BIGINT NOT NULL,
						password TEXT NOT NULL,
						access_token TEXT NOT NULL,
							agent_token TEXT NOT NULL,
							owner_mxid TEXT NOT NULL,
							agent_room_id TEXT NOT NULL,
							system_room_id TEXT NOT NULL DEFAULT '',
						user_id TEXT NOT NULL,
					display_name TEXT NOT NULL,
					domain TEXT NOT NULL,
					avatar_url TEXT NOT NULL,
					gender TEXT NOT NULL,
					birthday TEXT NOT NULL,
					phone TEXT NOT NULL,
					email TEXT NOT NULL
				)`,
				`CREATE TABLE IF NOT EXISTS p2p_read_markers (
					room_id TEXT PRIMARY KEY NOT NULL,
					event_id TEXT NOT NULL,
					origin_server_ts BIGINT NOT NULL
				)`,
				`CREATE TABLE IF NOT EXISTS p2p_channels (
					channel_id TEXT PRIMARY KEY NOT NULL,
					room_id TEXT NOT NULL,
					name TEXT NOT NULL,
					description TEXT NOT NULL,
					avatar_url TEXT NOT NULL,
					visibility TEXT NOT NULL,
					join_policy TEXT NOT NULL,
					channel_type TEXT NOT NULL,
					comments_enabled BIGINT NOT NULL,
					member_count BIGINT NOT NULL,
					pending_join_count BIGINT NOT NULL
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_channels_room_idx ON p2p_channels(room_id)`,
				`CREATE INDEX IF NOT EXISTS p2p_channels_type_visibility_idx ON p2p_channels(channel_type, visibility, channel_id)`,
				`CREATE TABLE IF NOT EXISTS p2p_channel_posts (
					post_id TEXT PRIMARY KEY NOT NULL,
					channel_id TEXT NOT NULL,
					room_id TEXT NOT NULL,
					event_id TEXT NOT NULL,
					author_mxid TEXT NOT NULL,
					author_name TEXT NOT NULL,
					body TEXT NOT NULL,
					message_type TEXT NOT NULL,
					media_json TEXT NOT NULL,
					visibility TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('public', 'private')),
					origin_server_ts BIGINT NOT NULL,
					comment_count BIGINT NOT NULL
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_channel_posts_channel_idx ON p2p_channel_posts(channel_id, origin_server_ts)`,
				`CREATE INDEX IF NOT EXISTS p2p_channel_posts_event_idx ON p2p_channel_posts(event_id)`,
				`CREATE INDEX IF NOT EXISTS p2p_channel_posts_author_idx ON p2p_channel_posts(author_mxid, origin_server_ts)`,
				`CREATE INDEX IF NOT EXISTS p2p_channel_posts_channel_visibility_idx ON p2p_channel_posts(channel_id, visibility, origin_server_ts DESC, post_id DESC)`,
				`CREATE TABLE IF NOT EXISTS p2p_channel_comments (
					comment_id TEXT PRIMARY KEY NOT NULL,
					post_id TEXT NOT NULL,
					channel_id TEXT NOT NULL,
					event_id TEXT NOT NULL,
					author_mxid TEXT NOT NULL,
					author_name TEXT NOT NULL,
					body TEXT NOT NULL,
					message_type TEXT NOT NULL,
					origin_server_ts BIGINT NOT NULL,
					reaction_count BIGINT NOT NULL,
					reacted_by_me BIGINT NOT NULL
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_channel_comments_post_idx ON p2p_channel_comments(post_id, origin_server_ts)`,
				`CREATE INDEX IF NOT EXISTS p2p_channel_comments_channel_idx ON p2p_channel_comments(channel_id, origin_server_ts)`,
				`CREATE INDEX IF NOT EXISTS p2p_channel_comments_event_idx ON p2p_channel_comments(event_id)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: integrated appservice tables v2",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`CREATE TABLE IF NOT EXISTS p2p_contacts (
					room_id TEXT PRIMARY KEY NOT NULL,
					peer_mxid TEXT NOT NULL,
					display_name TEXT NOT NULL,
					display_name_override BOOLEAN NOT NULL DEFAULT FALSE,
					remark TEXT NOT NULL DEFAULT '',
					domain TEXT NOT NULL,
					status TEXT NOT NULL
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_contacts_peer_idx ON p2p_contacts(peer_mxid)`,
				`CREATE INDEX IF NOT EXISTS p2p_contacts_status_idx ON p2p_contacts(status, domain)`,
				`CREATE TABLE IF NOT EXISTS p2p_groups (
					room_id TEXT PRIMARY KEY NOT NULL,
					name TEXT NOT NULL,
					topic TEXT NOT NULL,
					avatar_url TEXT NOT NULL,
					member_count BIGINT NOT NULL,
					invite_policy TEXT NOT NULL
				)`,
				`CREATE TABLE IF NOT EXISTS p2p_calls (
					call_id TEXT PRIMARY KEY NOT NULL,
					room_id TEXT NOT NULL,
					room_type TEXT NOT NULL,
					media_type TEXT NOT NULL,
					created_by_mxid TEXT NOT NULL,
					state TEXT NOT NULL,
					created_at TEXT NOT NULL,
					answered_at TEXT NOT NULL DEFAULT '',
					ended_at TEXT NOT NULL DEFAULT '',
					ended_by_mxid TEXT NOT NULL DEFAULT '',
					end_reason TEXT NOT NULL DEFAULT '',
					duration_ms BIGINT NOT NULL DEFAULT 0
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_calls_room_idx ON p2p_calls(room_id, created_at)`,
				`CREATE INDEX IF NOT EXISTS p2p_calls_state_idx ON p2p_calls(state, created_at)`,
				`CREATE TABLE IF NOT EXISTS p2p_favorites (
					id BIGINT PRIMARY KEY NOT NULL,
					event_id TEXT NOT NULL,
					room_id TEXT NOT NULL,
					sender_id TEXT NOT NULL,
					sender_name TEXT NOT NULL,
					content TEXT NOT NULL,
					message_type TEXT NOT NULL,
					origin_server_ts BIGINT NOT NULL,
					created_at TEXT NOT NULL
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_favorites_type_idx ON p2p_favorites(message_type, created_at)`,
				`CREATE INDEX IF NOT EXISTS p2p_favorites_event_idx ON p2p_favorites(event_id)`,
				`CREATE TABLE IF NOT EXISTS p2p_follows (
					domain TEXT PRIMARY KEY NOT NULL,
					created_at TEXT NOT NULL
				)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: integrated appservice tables v3",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`CREATE TABLE IF NOT EXISTS p2p_reactions (
					target_type TEXT NOT NULL,
					target_id TEXT NOT NULL,
					channel_id TEXT NOT NULL,
					post_id TEXT NOT NULL,
					comment_id TEXT NOT NULL,
					reaction TEXT NOT NULL,
					user_id TEXT NOT NULL,
					active BIGINT NOT NULL,
					created_at TEXT NOT NULL,
					PRIMARY KEY (target_type, target_id, reaction, user_id)
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_reactions_user_idx ON p2p_reactions(user_id, active)`,
				`CREATE INDEX IF NOT EXISTS p2p_reactions_target_idx ON p2p_reactions(target_type, target_id, reaction, active)`,
				`CREATE TABLE IF NOT EXISTS p2p_members (
					room_id TEXT NOT NULL,
					user_id TEXT NOT NULL,
					channel_id TEXT NOT NULL,
					display_name TEXT NOT NULL,
					domain TEXT NOT NULL,
					membership TEXT NOT NULL,
					role TEXT NOT NULL,
					muted BIGINT NOT NULL,
					PRIMARY KEY (room_id, user_id)
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_members_channel_idx ON p2p_members(channel_id, membership)`,
				`CREATE INDEX IF NOT EXISTS p2p_members_room_idx ON p2p_members(room_id, membership)`,
				`CREATE INDEX IF NOT EXISTS p2p_members_user_idx ON p2p_members(user_id, membership)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: integrated appservice tables v4 member avatars",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			_, err := txn.ExecContext(ctx, `ALTER TABLE p2p_members ADD COLUMN avatar_url TEXT NOT NULL DEFAULT ''`)
			return err
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: integrated appservice tables v5 product mute state",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`ALTER TABLE p2p_channels ADD COLUMN muted BIGINT NOT NULL DEFAULT 0`,
				`ALTER TABLE p2p_groups ADD COLUMN muted BIGINT NOT NULL DEFAULT 0`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: integrated appservice tables v6 member join order",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`ALTER TABLE p2p_members ADD COLUMN joined_at BIGINT NOT NULL DEFAULT 0`,
				`CREATE INDEX IF NOT EXISTS p2p_members_room_joined_idx ON p2p_members(room_id, membership, joined_at, user_id)`,
				`CREATE INDEX IF NOT EXISTS p2p_members_channel_joined_idx ON p2p_members(channel_id, membership, joined_at, user_id)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: integrated appservice tables v7 portal matrix device",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			_, err := txn.ExecContext(ctx, `ALTER TABLE p2p_portal ADD COLUMN matrix_device_id TEXT NOT NULL DEFAULT ''`)
			return err
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: integrated appservice tables v11 channel comment replies",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`ALTER TABLE p2p_channel_comments ADD COLUMN reply_to_comment_id TEXT NOT NULL DEFAULT ''`,
				`ALTER TABLE p2p_channel_comments ADD COLUMN reply_to_author_mxid TEXT NOT NULL DEFAULT ''`,
				`ALTER TABLE p2p_channel_comments ADD COLUMN mentions_json TEXT NOT NULL DEFAULT '[]'`,
				`CREATE INDEX IF NOT EXISTS p2p_channel_comments_reply_idx ON p2p_channel_comments(post_id, reply_to_comment_id, origin_server_ts)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: integrated appservice tables v12 channel comment media",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			_, err := txn.ExecContext(ctx, `ALTER TABLE p2p_channel_comments ADD COLUMN media_json TEXT NOT NULL DEFAULT ''`)
			return err
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: integrated appservice tables v13 event outbox",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`CREATE TABLE IF NOT EXISTS p2p_events (
					seq BIGINT PRIMARY KEY NOT NULL,
					type TEXT NOT NULL,
					room_id TEXT NOT NULL,
					event_id TEXT NOT NULL,
					dedupe_key TEXT NOT NULL DEFAULT '',
					payload_json TEXT NOT NULL,
					created_at TEXT NOT NULL
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_events_room_idx ON p2p_events(room_id, seq)`,
				`CREATE INDEX IF NOT EXISTS p2p_events_type_idx ON p2p_events(type, seq)`,
				`CREATE UNIQUE INDEX IF NOT EXISTS p2p_events_dedupe_key_idx ON p2p_events(dedupe_key) WHERE dedupe_key <> ''`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: integrated appservice tables v14 channel invite grants",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`CREATE TABLE IF NOT EXISTS p2p_channel_invite_grants (
					grant_id TEXT PRIMARY KEY NOT NULL,
					channel_id TEXT NOT NULL,
					room_id TEXT NOT NULL,
					share_room_id TEXT NOT NULL,
					created_by TEXT NOT NULL,
					created_at BIGINT NOT NULL
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_channel_invite_grants_channel_idx ON p2p_channel_invite_grants(channel_id, share_room_id)`,
				`CREATE INDEX IF NOT EXISTS p2p_channel_invite_grants_room_idx ON p2p_channel_invite_grants(room_id, share_room_id)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: drop legacy message mirror table v15",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			_, err := txn.ExecContext(ctx, `DROP TABLE IF EXISTS p2p_messages`)
			return err
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: unique contact peer v16",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`DELETE FROM p2p_contacts
				 WHERE room_id IN (
					SELECT room_id FROM (
						SELECT room_id,
							ROW_NUMBER() OVER (
								PARTITION BY peer_mxid
								ORDER BY
									CASE LOWER(TRIM(status))
										WHEN 'accepted' THEN 5
										WHEN 'pending_inbound' THEN 4
										WHEN 'pending_outbound' THEN 3
										WHEN 'rejected' THEN 2
										WHEN 'reject' THEN 2
										WHEN 'deleted' THEN 1
										ELSE 0
									END DESC,
									room_id ASC
							) AS duplicate_rank
						FROM p2p_contacts
					) ranked
					WHERE duplicate_rank > 1
				)`,
				`DROP INDEX IF EXISTS p2p_contacts_peer_idx`,
				`CREATE UNIQUE INDEX IF NOT EXISTS p2p_contacts_peer_idx ON p2p_contacts(peer_mxid)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: product conversations v17",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`CREATE TABLE IF NOT EXISTS p2p_conversations (
					conversation_id TEXT PRIMARY KEY NOT NULL,
					matrix_room_id TEXT NOT NULL UNIQUE,
					kind TEXT NOT NULL,
					lifecycle TEXT NOT NULL,
						created_by_mxid TEXT NOT NULL DEFAULT '',
						peer_mxid TEXT NOT NULL DEFAULT '',
						title TEXT NOT NULL DEFAULT '',
						avatar_url TEXT NOT NULL DEFAULT '',
						last_event_id TEXT NOT NULL DEFAULT '',
						last_message TEXT NOT NULL DEFAULT '',
						last_activity_at BIGINT NOT NULL DEFAULT 0,
					projection_state TEXT NOT NULL DEFAULT 'ready',
					projection_reason TEXT NOT NULL DEFAULT '',
					created_at BIGINT NOT NULL,
					updated_at BIGINT NOT NULL
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_conversations_kind_idx ON p2p_conversations(kind)`,
				`CREATE INDEX IF NOT EXISTS p2p_conversations_updated_idx ON p2p_conversations(updated_at)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: conversation peer mxid v18",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			_, err := txn.ExecContext(ctx, `ALTER TABLE p2p_conversations ADD COLUMN IF NOT EXISTS peer_mxid TEXT NOT NULL DEFAULT ''`)
			return err
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: backfill product conversations v19",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return backfillProductConversations(ctx, txn)
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: conversation last message v20",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			if _, err := txn.ExecContext(ctx, `ALTER TABLE p2p_conversations ADD COLUMN IF NOT EXISTS last_message TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
			_, err := txn.ExecContext(ctx, `
					DO $$
					BEGIN
						IF to_regclass('public.syncapi_output_room_events') IS NOT NULL THEN
							WITH latest AS (
								SELECT DISTINCT ON (room_id)
									room_id,
									event_id,
									COALESCE((headered_event_json::jsonb->>'origin_server_ts')::bigint, 0) AS origin_server_ts,
									COALESCE(
										NULLIF(TRIM(headered_event_json::jsonb->'content'->>'body'), ''),
										CASE LOWER(TRIM(headered_event_json::jsonb->'content'->>'msgtype'))
											WHEN 'm.image' THEN '图片'
											WHEN 'image' THEN '图片'
											WHEN 'm.video' THEN '视频'
											WHEN 'video' THEN '视频'
											WHEN 'm.audio' THEN '语音'
											WHEN 'audio' THEN '语音'
											WHEN 'm.file' THEN '文件'
											WHEN 'file' THEN '文件'
											ELSE ''
										END
									) AS last_message
								FROM syncapi_output_room_events
								WHERE type = 'm.room.message'
									AND COALESCE(exclude_from_sync, false) = false
								ORDER BY room_id, id DESC
							)
							UPDATE p2p_conversations c
							SET
								last_event_id = CASE WHEN latest.event_id <> '' THEN latest.event_id ELSE c.last_event_id END,
								last_message = CASE WHEN latest.last_message <> '' THEN latest.last_message ELSE c.last_message END,
								last_activity_at = CASE WHEN latest.origin_server_ts > c.last_activity_at THEN latest.origin_server_ts ELSE c.last_activity_at END,
								updated_at = CASE WHEN latest.origin_server_ts > c.updated_at THEN latest.origin_server_ts ELSE c.updated_at END
							FROM latest
							WHERE c.matrix_room_id = latest.room_id
								AND latest.origin_server_ts >= c.last_activity_at;
						END IF;
					END $$;
				`)
			return err
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: member requester node v21",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			exists, err := productTableExists(ctx, txn, "p2p_members")
			if err != nil {
				return err
			}
			if !exists {
				return nil
			}
			_, err = txn.ExecContext(ctx, `ALTER TABLE p2p_members ADD COLUMN requester_node_base_url TEXT NOT NULL DEFAULT ''`)
			return err
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: contact avatars v22",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			exists, err := productTableExists(ctx, txn, "p2p_contacts")
			if err != nil {
				return err
			}
			if !exists {
				return nil
			}
			_, err = txn.ExecContext(ctx, `ALTER TABLE p2p_contacts ADD COLUMN avatar_url TEXT NOT NULL DEFAULT ''`)
			return err
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: call lifecycle fields v23",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			exists, err := productTableExists(ctx, txn, "p2p_calls")
			if err != nil {
				return err
			}
			if !exists {
				return nil
			}
			return execMigrationStatements(ctx, txn, []string{
				`ALTER TABLE p2p_calls ADD COLUMN IF NOT EXISTS answered_at TEXT NOT NULL DEFAULT ''`,
				`ALTER TABLE p2p_calls ADD COLUMN IF NOT EXISTS ended_at TEXT NOT NULL DEFAULT ''`,
				`ALTER TABLE p2p_calls ADD COLUMN IF NOT EXISTS ended_by_mxid TEXT NOT NULL DEFAULT ''`,
				`ALTER TABLE p2p_calls ADD COLUMN IF NOT EXISTS end_reason TEXT NOT NULL DEFAULT ''`,
				`ALTER TABLE p2p_calls ADD COLUMN IF NOT EXISTS duration_ms BIGINT NOT NULL DEFAULT 0`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: contact request remark v24",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			exists, err := productTableExists(ctx, txn, "p2p_contacts")
			if err != nil {
				return err
			}
			if !exists {
				return nil
			}
			_, err = txn.ExecContext(ctx, `ALTER TABLE p2p_contacts ADD COLUMN IF NOT EXISTS remark TEXT NOT NULL DEFAULT ''`)
			return err
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: owner scoped member indexes v25",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			exists, err := productTableExists(ctx, txn, "p2p_members")
			if err != nil {
				return err
			}
			if !exists {
				return nil
			}
			return execMigrationStatements(ctx, txn, []string{
				`CREATE INDEX IF NOT EXISTS p2p_members_user_room_idx ON p2p_members(user_id, membership, room_id)`,
				`CREATE INDEX IF NOT EXISTS p2p_members_user_channel_idx ON p2p_members(user_id, membership, channel_id)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: public channel visibility index v26",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			exists, err := productTableExists(ctx, txn, "p2p_channels")
			if err != nil {
				return err
			}
			if !exists {
				return nil
			}
			_, err = txn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS p2p_channels_visibility_idx ON p2p_channels(visibility, channel_id)`)
			return err
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: event dedupe key v27",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			exists, err := productTableExists(ctx, txn, "p2p_events")
			if err != nil {
				return err
			}
			if !exists {
				return nil
			}
			return execMigrationStatements(ctx, txn, []string{
				`ALTER TABLE p2p_events ADD COLUMN IF NOT EXISTS dedupe_key TEXT NOT NULL DEFAULT ''`,
				`CREATE UNIQUE INDEX IF NOT EXISTS p2p_events_dedupe_key_idx ON p2p_events(dedupe_key) WHERE dedupe_key <> ''`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: contact display name override v28",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			exists, err := productTableExists(ctx, txn, "p2p_contacts")
			if err != nil {
				return err
			}
			if !exists {
				return nil
			}
			_, err = txn.ExecContext(ctx, `ALTER TABLE p2p_contacts ADD COLUMN IF NOT EXISTS display_name_override BOOLEAN NOT NULL DEFAULT FALSE`)
			return err
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: portal agent config json v29",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			exists, err := productTableExists(ctx, txn, "p2p_portal")
			if err != nil {
				return err
			}
			if !exists {
				return nil
			}
			_, err = txn.ExecContext(ctx, `ALTER TABLE p2p_portal ADD COLUMN IF NOT EXISTS agent_config_json TEXT NOT NULL DEFAULT ''`)
			return err
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: owner blocks v30",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`CREATE TABLE IF NOT EXISTS p2p_blocks (
					target_type TEXT NOT NULL,
					target_id TEXT NOT NULL,
					room_id TEXT NOT NULL DEFAULT '',
					peer_mxid TEXT NOT NULL DEFAULT '',
					display_name TEXT NOT NULL DEFAULT '',
					avatar_url TEXT NOT NULL DEFAULT '',
					created_at BIGINT NOT NULL DEFAULT 0,
					PRIMARY KEY (target_type, target_id)
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_blocks_type_idx ON p2p_blocks(target_type, display_name, target_id)`,
				`CREATE INDEX IF NOT EXISTS p2p_blocks_room_idx ON p2p_blocks(room_id)`,
				`CREATE INDEX IF NOT EXISTS p2p_blocks_peer_idx ON p2p_blocks(peer_mxid)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: official plugins v31",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`CREATE TABLE IF NOT EXISTS p2p_plugins (
					id TEXT PRIMARY KEY NOT NULL,
					name TEXT NOT NULL DEFAULT '',
					version TEXT NOT NULL DEFAULT '',
					image TEXT NOT NULL DEFAULT '',
					digest TEXT NOT NULL DEFAULT '',
					status TEXT NOT NULL DEFAULT '',
					enabled BIGINT NOT NULL DEFAULT 0,
					config_json TEXT NOT NULL DEFAULT '',
					last_job_id TEXT NOT NULL DEFAULT '',
					created_at BIGINT NOT NULL DEFAULT 0,
					updated_at BIGINT NOT NULL DEFAULT 0
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_plugins_status_idx ON p2p_plugins(status, enabled)`,
				`CREATE TABLE IF NOT EXISTS p2p_plugin_jobs (
					job_id TEXT PRIMARY KEY NOT NULL,
					plugin_id TEXT NOT NULL,
					action TEXT NOT NULL,
					status TEXT NOT NULL,
					message TEXT NOT NULL DEFAULT '',
					created_at BIGINT NOT NULL DEFAULT 0,
					updated_at BIGINT NOT NULL DEFAULT 0
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_plugin_jobs_plugin_idx ON p2p_plugin_jobs(plugin_id, created_at)`,
				`CREATE INDEX IF NOT EXISTS p2p_plugin_jobs_status_idx ON p2p_plugin_jobs(status, updated_at)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: plugin secrets v32",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`CREATE TABLE IF NOT EXISTS p2p_plugin_secrets (
					plugin_id TEXT NOT NULL,
					name TEXT NOT NULL,
					value TEXT NOT NULL DEFAULT '',
					updated_at BIGINT NOT NULL DEFAULT 0,
					PRIMARY KEY (plugin_id, name)
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_plugin_secrets_updated_idx ON p2p_plugin_secrets(plugin_id, updated_at)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: system reports v33",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			exists, err := productTableExists(ctx, txn, "p2p_portal")
			if err != nil {
				return err
			}
			if exists {
				if _, err = txn.ExecContext(ctx, `ALTER TABLE p2p_portal ADD COLUMN IF NOT EXISTS system_room_id TEXT NOT NULL DEFAULT ''`); err != nil {
					return err
				}
			}
			return execMigrationStatements(ctx, txn, []string{
				`CREATE TABLE IF NOT EXISTS p2p_reports (
					report_id TEXT PRIMARY KEY NOT NULL,
					target_type TEXT NOT NULL,
					target_room_id TEXT NOT NULL,
					target_channel_id TEXT NOT NULL DEFAULT '',
					target_name TEXT NOT NULL DEFAULT '',
					reporter_mxid TEXT NOT NULL DEFAULT '',
					reporter_display_name TEXT NOT NULL DEFAULT '',
					reason TEXT NOT NULL DEFAULT '',
					body TEXT NOT NULL DEFAULT '',
					image_urls_json TEXT NOT NULL DEFAULT '[]',
					system_room_id TEXT NOT NULL DEFAULT '',
					event_id TEXT NOT NULL DEFAULT '',
					origin_server_ts BIGINT NOT NULL DEFAULT 0,
					created_at TEXT NOT NULL DEFAULT ''
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_reports_target_idx ON p2p_reports(target_type, target_room_id, created_at)`,
				`CREATE INDEX IF NOT EXISTS p2p_reports_reporter_idx ON p2p_reports(reporter_mxid, created_at)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: portal client build v34",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			exists, err := productTableExists(ctx, txn, "p2p_portal")
			if err != nil || !exists {
				return err
			}
			return execMigrationStatements(ctx, txn, []string{
				`ALTER TABLE p2p_portal ADD COLUMN IF NOT EXISTS client_version TEXT NOT NULL DEFAULT ''`,
				`ALTER TABLE p2p_portal ADD COLUMN IF NOT EXISTS client_build_number TEXT NOT NULL DEFAULT ''`,
				`ALTER TABLE p2p_portal ADD COLUMN IF NOT EXISTS client_platform TEXT NOT NULL DEFAULT ''`,
				`ALTER TABLE p2p_portal ADD COLUMN IF NOT EXISTS client_version_reported_at TEXT NOT NULL DEFAULT ''`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: recoverable operations v35",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			statements := make([]string, 0, 5)
			contactsExist, err := productTableExists(ctx, txn, "p2p_contacts")
			if err != nil {
				return err
			}
			if contactsExist {
				statements = append(statements, `ALTER TABLE p2p_contacts ADD COLUMN IF NOT EXISTS request_id TEXT NOT NULL DEFAULT ''`)
			}
			membersExist, err := productTableExists(ctx, txn, "p2p_members")
			if err != nil {
				return err
			}
			if membersExist {
				statements = append(statements, `ALTER TABLE p2p_members ADD COLUMN IF NOT EXISTS request_id TEXT NOT NULL DEFAULT ''`)
			}
			statements = append(statements,
				`CREATE TABLE IF NOT EXISTS p2p_operations (
					operation_id TEXT PRIMARY KEY NOT NULL,
					action TEXT NOT NULL DEFAULT '',
					status TEXT NOT NULL DEFAULT '',
					phase TEXT NOT NULL DEFAULT '',
					room_id TEXT NOT NULL DEFAULT '',
					current_room_id TEXT NOT NULL DEFAULT '',
					user_id TEXT NOT NULL DEFAULT '',
					peer_mxid TEXT NOT NULL DEFAULT '',
					request_id TEXT NOT NULL DEFAULT '',
					result_json TEXT NOT NULL DEFAULT '',
					error_code TEXT NOT NULL DEFAULT '',
					created_at BIGINT NOT NULL DEFAULT 0,
					updated_at BIGINT NOT NULL DEFAULT 0
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_operations_status_updated_idx ON p2p_operations(status, updated_at)`,
			)
			return execMigrationStatements(ctx, txn, statements)
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: recoverable operation claims v36",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`ALTER TABLE p2p_operations ADD COLUMN IF NOT EXISTS revision BIGINT NOT NULL DEFAULT 0`,
				`ALTER TABLE p2p_operations ADD COLUMN IF NOT EXISTS lease_owner TEXT NOT NULL DEFAULT ''`,
				`ALTER TABLE p2p_operations ADD COLUMN IF NOT EXISTS lease_until BIGINT NOT NULL DEFAULT 0`,
				`CREATE INDEX IF NOT EXISTS p2p_operations_lease_idx ON p2p_operations(lease_until) WHERE lease_owner <> ''`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: operation base generations v37",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`ALTER TABLE p2p_operations ADD COLUMN IF NOT EXISTS base_request_id TEXT NOT NULL DEFAULT ''`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: legacy agent invocation reservations v38",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`CREATE TABLE IF NOT EXISTS p2p_legacy_agent_invocations (
					matrix_room_id TEXT NOT NULL CHECK (matrix_room_id <> ''),
					request_id UUID NOT NULL,
					matrix_invoke_event_id TEXT NOT NULL CHECK (matrix_invoke_event_id <> ''),
					matrix_input_event_id TEXT NOT NULL CHECK (matrix_input_event_id <> ''),
					tenant_id UUID NOT NULL,
					installation_id UUID NOT NULL,
					conversation_id UUID NOT NULL,
					request_event_id UUID NOT NULL,
					source_digest BYTEA NOT NULL CHECK (octet_length(source_digest) = 32),
					idempotency_digest BYTEA NOT NULL CHECK (octet_length(idempotency_digest) = 32),
					request_digest BYTEA NOT NULL CHECK (octet_length(request_digest) = 32),
					preferred_connector_id UUID,
					required_capabilities TEXT[] NOT NULL DEFAULT '{}'
						CHECK (cardinality(required_capabilities) <= 64),
					dispatch_mode TEXT NOT NULL CHECK (dispatch_mode IN ('single', 'failover')),
					grant_version BIGINT NOT NULL CHECK (grant_version > 0 AND grant_version <= 9007199254740991),
					state TEXT NOT NULL CHECK (state IN ('reserved', 'accepted', 'rejected')),
					run_id UUID,
					routing_state TEXT NOT NULL DEFAULT ''
						CHECK (routing_state IN ('', 'queued', 'offered', 'leased', 'reconcile_required', 'expired')),
					inserted BOOLEAN,
					error_code TEXT NOT NULL DEFAULT '',
					created_at TIMESTAMPTZ NOT NULL,
					updated_at TIMESTAMPTZ NOT NULL,
					CHECK (
						(state = 'reserved' AND run_id IS NULL AND routing_state = '' AND inserted IS NULL AND error_code = '')
						OR (state = 'accepted' AND run_id IS NOT NULL AND routing_state <> '' AND inserted IS NOT NULL AND error_code = '')
						OR (state = 'rejected' AND run_id IS NULL AND routing_state = '' AND inserted IS NULL AND error_code <> '')
					),
					PRIMARY KEY (matrix_room_id, request_id),
					UNIQUE (matrix_invoke_event_id),
					UNIQUE (tenant_id, matrix_room_id, idempotency_digest)
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_legacy_agent_invocations_state_updated_idx
					ON p2p_legacy_agent_invocations(state, updated_at)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: authoritative read marker order v73",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			exists, err := productTableExists(ctx, txn, "p2p_read_markers")
			if err != nil || !exists {
				return err
			}
			return execMigrationStatements(ctx, txn, []string{
				`ALTER TABLE p2p_read_markers ADD COLUMN IF NOT EXISTS topological_position BIGINT NOT NULL DEFAULT 0`,
				`ALTER TABLE p2p_read_markers ADD COLUMN IF NOT EXISTS stream_position BIGINT NOT NULL DEFAULT 0`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: channel favorite reaction backfill v74",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return backfillLegacyChannelFavorites(ctx, txn)
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: projected reaction event identity v75",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			exists, err := productTableExists(ctx, txn, "p2p_reactions")
			if err != nil || !exists {
				return err
			}
			return execMigrationStatements(ctx, txn, []string{
				`ALTER TABLE p2p_reactions ADD COLUMN IF NOT EXISTS event_id TEXT NOT NULL DEFAULT ''`,
				`CREATE INDEX IF NOT EXISTS p2p_reactions_event_idx ON p2p_reactions(event_id) WHERE event_id <> ''`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: durable native agent turns v77",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`CREATE TABLE IF NOT EXISTS p2p_native_agent_turns (
					owner_id TEXT NOT NULL CHECK (owner_id <> ''),
					turn_id TEXT NOT NULL CHECK (turn_id <> ''),
					conversation_id TEXT NOT NULL CHECK (conversation_id <> ''),
					action TEXT NOT NULL CHECK (action <> ''),
					request_digest BYTEA NOT NULL CHECK (octet_length(request_digest) = 32),
					state TEXT NOT NULL CHECK (state IN ('accepted', 'running', 'succeeded', 'failed', 'stopped', 'interrupted')),
					error_message TEXT NOT NULL DEFAULT '',
					created_at TIMESTAMPTZ NOT NULL,
					updated_at TIMESTAMPTZ NOT NULL,
					PRIMARY KEY (owner_id, turn_id)
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_native_agent_turns_owner_conversation_idx
					ON p2p_native_agent_turns(owner_id, conversation_id, created_at, turn_id)`,
				`CREATE INDEX IF NOT EXISTS p2p_native_agent_turns_active_idx
					ON p2p_native_agent_turns(state, updated_at) WHERE state IN ('accepted', 'running')`,
				`CREATE TABLE IF NOT EXISTS p2p_native_agent_turn_events (
					owner_id TEXT NOT NULL,
					turn_id TEXT NOT NULL,
					conversation_id TEXT NOT NULL CHECK (conversation_id <> ''),
					seq BIGINT NOT NULL CHECK (seq > 0),
					kind TEXT NOT NULL CHECK (kind IN ('runtime', 'error')),
					event_name TEXT NOT NULL CHECK (event_name <> ''),
					data_json JSONB NOT NULL DEFAULT '{}'::jsonb,
					created_at TIMESTAMPTZ NOT NULL,
					PRIMARY KEY (owner_id, turn_id, seq),
					FOREIGN KEY (owner_id, turn_id) REFERENCES p2p_native_agent_turns(owner_id, turn_id) ON DELETE CASCADE
				)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: canonical Matrix member membership v76",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			exists, err := productTableExists(ctx, txn, "p2p_members")
			if err != nil || !exists {
				return err
			}
			_, err = txn.ExecContext(ctx, `
				UPDATE p2p_members
				SET membership = CASE
					WHEN LOWER(BTRIM(membership)) = 'joined' THEN 'join'
					ELSE LOWER(BTRIM(membership))
				END
				WHERE membership <> CASE
					WHEN LOWER(BTRIM(membership)) = 'joined' THEN 'join'
					ELSE LOWER(BTRIM(membership))
				END
			`)
			return err
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: agent and execution v2 fresh schema v78",
		Up:      agentAndExecutionV2FreshSchema,
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: execution run stage terminal immutability v79",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`CREATE OR REPLACE FUNCTION core_execution_run_stage_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ DECLARE reconciled BOOLEAN; BEGIN IF TG_OP='DELETE' THEN RAISE EXCEPTION 'execution run stage identity/state is immutable'; END IF; SELECT EXISTS(SELECT 1 FROM core_execution_reconciliation_resolutions r WHERE r.owner_id=OLD.owner_id AND r.run_id=OLD.run_id AND r.stage_id=OLD.stage_id AND r.outcome=NEW.status) INTO reconciled; IF NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.stage_id IS DISTINCT FROM OLD.stage_id OR NEW.project_id IS DISTINCT FROM OLD.project_id OR NEW.plan_id IS DISTINCT FROM OLD.plan_id OR NEW.plan_revision IS DISTINCT FROM OLD.plan_revision OR NEW.plan_digest IS DISTINCT FROM OLD.plan_digest OR NEW.run_revision IS DISTINCT FROM OLD.run_revision OR NEW.plan_stage_key IS DISTINCT FROM OLD.plan_stage_key OR NEW.stage_revision IS DISTINCT FROM OLD.stage_revision OR NEW.plan_stage_digest IS DISTINCT FROM OLD.plan_stage_digest OR NEW.target_id IS DISTINCT FROM OLD.target_id OR NEW.target_revision IS DISTINCT FROM OLD.target_revision OR NEW.target_digest IS DISTINCT FROM OLD.target_digest OR NEW.created_at IS DISTINCT FROM OLD.created_at OR NEW.revision IS DISTINCT FROM OLD.revision+1 OR (OLD.status <> 'blocked' AND (NEW.task_id IS DISTINCT FROM OLD.task_id OR NEW.confirmation_id IS DISTINCT FROM OLD.confirmation_id)) OR (OLD.started_at IS NOT NULL AND NEW.started_at IS DISTINCT FROM OLD.started_at) OR (OLD.completed_at IS NOT NULL AND NEW.completed_at IS DISTINCT FROM OLD.completed_at) OR OLD.status IN ('succeeded','failed','skipped','canceled','rejected','expired') OR (OLD.status IN ('running','succeeded','failed','uncertain','skipped','canceled','rejected','expired') AND NEW.status IN ('blocked','waiting_user','queued')) OR (OLD.status='uncertain' AND (NOT reconciled OR NEW.status NOT IN ('succeeded','failed','canceled'))) THEN RAISE EXCEPTION 'execution run stage identity/state is immutable'; END IF; RETURN NEW; END $$`,
				`DROP TRIGGER IF EXISTS core_execution_run_stages_immutable ON core_execution_run_stages`,
				`CREATE TRIGGER core_execution_run_stages_immutable BEFORE UPDATE OR DELETE ON core_execution_run_stages FOR EACH ROW EXECUTE FUNCTION core_execution_run_stage_immutable()`,
			})
		},
	})
	return m.Up(ctx)
}

func agentAndExecutionV2FreshSchema(ctx context.Context, txn *sql.Tx) error {
	channelPostsExist, err := productTableExists(ctx, txn, "p2p_channel_posts")
	if err != nil {
		return err
	}
	if channelPostsExist {
		if err := execMigrationStatements(ctx, txn, []string{
			`ALTER TABLE p2p_channel_posts ADD COLUMN IF NOT EXISTS visibility TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('public', 'private'))`,
			`CREATE INDEX IF NOT EXISTS p2p_channel_posts_channel_visibility_idx ON p2p_channel_posts(channel_id, visibility, origin_server_ts DESC, post_id DESC)`,
		}); err != nil {
			return err
		}
	}
	if err := execMigrationStatements(ctx, txn, []string{
		`ALTER TABLE p2p_native_agent_turns ADD COLUMN IF NOT EXISTS model_profile_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE p2p_native_agent_turns ADD COLUMN IF NOT EXISTS model_profile_revision BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE p2p_native_agent_turns ADD COLUMN IF NOT EXISTS credential_version BIGINT NOT NULL DEFAULT 0`,
	}); err != nil {
		return err
	}
	if err := execMigrationDDL(ctx, txn, AgentTaskDDL); err != nil {
		return err
	}
	if err := execMigrationDDL(ctx, txn, AgentConfirmationDDL); err != nil {
		return err
	}
	if err := execMigrationDDL(ctx, txn, AgentExtensionDDL); err != nil {
		return err
	}
	if err := execMigrationStatements(ctx, txn, []string{
		`CREATE TABLE IF NOT EXISTS p2p_agent_core_turns (owner_id TEXT NOT NULL CHECK(owner_id<>''), client_turn_id TEXT NOT NULL CHECK(client_turn_id<>''), core_turn_id TEXT NOT NULL DEFAULT '', core_profile_id TEXT NOT NULL DEFAULT '', conversation_id TEXT NOT NULL DEFAULT '', request_digest BYTEA NOT NULL CHECK(octet_length(request_digest)=32), status TEXT NOT NULL CHECK(status<>''), last_sequence BIGINT NOT NULL DEFAULT 0 CHECK(last_sequence>=0), core_revision BIGINT NOT NULL DEFAULT 0, model_profile_revision BIGINT NOT NULL DEFAULT 0, last_event_kind TEXT NOT NULL DEFAULT '', terminal_code TEXT NOT NULL DEFAULT '', terminal_summary TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, PRIMARY KEY(owner_id,client_turn_id))`,
		`CREATE INDEX IF NOT EXISTS p2p_agent_core_turns_owner_conversation_idx ON p2p_agent_core_turns(owner_id,conversation_id,created_at,client_turn_id)`,
		`CREATE TABLE IF NOT EXISTS p2p_native_agent_conversations (owner_id TEXT NOT NULL CHECK(owner_id<>''),conversation_id TEXT NOT NULL CHECK(char_length(conversation_id) BETWEEN 1 AND 256),title TEXT NOT NULL DEFAULT '',active BOOLEAN NOT NULL DEFAULT TRUE,deleted BOOLEAN NOT NULL DEFAULT FALSE,revision BIGINT NOT NULL DEFAULT 1 CHECK(revision>0),last_message_seq BIGINT NOT NULL DEFAULT 0 CHECK(last_message_seq>=0),summary TEXT NOT NULL DEFAULT '',summary_through_seq BIGINT NOT NULL DEFAULT 0 CHECK(summary_through_seq>=0),created_at TIMESTAMPTZ NOT NULL,updated_at TIMESTAMPTZ NOT NULL,deleted_at TIMESTAMPTZ,PRIMARY KEY(owner_id,conversation_id))`,
		`CREATE TABLE IF NOT EXISTS p2p_native_agent_messages (owner_id TEXT NOT NULL,conversation_id TEXT NOT NULL,seq BIGINT NOT NULL CHECK(seq>0),turn_id TEXT NOT NULL DEFAULT '',message_id TEXT NOT NULL,role TEXT NOT NULL CHECK(role IN ('user','assistant')),content TEXT NOT NULL,references_json JSONB NOT NULL DEFAULT '[]'::jsonb,created_at TIMESTAMPTZ NOT NULL,PRIMARY KEY(owner_id,conversation_id,seq),UNIQUE(owner_id,conversation_id,message_id),FOREIGN KEY(owner_id,conversation_id) REFERENCES p2p_native_agent_conversations(owner_id,conversation_id) ON DELETE CASCADE)`,
		`CREATE TABLE IF NOT EXISTS p2p_native_agent_memory_records (owner_id TEXT NOT NULL,memory_id TEXT NOT NULL,title TEXT NOT NULL,content TEXT NOT NULL,tags_json JSONB NOT NULL DEFAULT '[]'::jsonb,request_digest BYTEA NOT NULL CHECK(octet_length(request_digest)=32),revision BIGINT NOT NULL DEFAULT 1 CHECK(revision>0),source_id TEXT,chunk_ordinal BIGINT,created_at TIMESTAMPTZ NOT NULL,updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),PRIMARY KEY(owner_id,memory_id))`,
		`CREATE TABLE IF NOT EXISTS p2p_agent_schedules (schedule_id TEXT NOT NULL,owner_id TEXT NOT NULL,name TEXT NOT NULL,prompt TEXT NOT NULL,trigger_kind TEXT NOT NULL,trigger_value TEXT NOT NULL,timezone TEXT NOT NULL,skip_if_running BOOLEAN NOT NULL DEFAULT FALSE,status TEXT NOT NULL,revision BIGINT NOT NULL DEFAULT 1,model_profile_id TEXT NOT NULL,model_profile_revision BIGINT NOT NULL,credential_version BIGINT NOT NULL,next_run_at TIMESTAMPTZ,latest_run_at TIMESTAMPTZ,lease_owner TEXT NOT NULL DEFAULT '',lease_until TIMESTAMPTZ,lease_epoch BIGINT NOT NULL DEFAULT 0,idempotency_key TEXT NOT NULL DEFAULT '',task_template JSONB NOT NULL DEFAULT '{}'::jsonb,core_state TEXT NOT NULL DEFAULT 'active' CHECK(core_state IN ('active','paused')),trigger_json JSONB NOT NULL DEFAULT '{}'::jsonb,deleted_at TIMESTAMPTZ,created_at TIMESTAMPTZ NOT NULL,updated_at TIMESTAMPTZ NOT NULL,PRIMARY KEY(owner_id,schedule_id))`,
		`CREATE TABLE IF NOT EXISTS p2p_agent_model_profiles (owner_id TEXT NOT NULL CHECK(owner_id<>''), profile_id TEXT NOT NULL CHECK(profile_id<>''), client_profile_id TEXT NOT NULL CHECK(client_profile_id<>''), display_name TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL, base_url TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', system_prompt TEXT NOT NULL DEFAULT '', temperature DOUBLE PRECISION, top_p DOUBLE PRECISION, max_output_tokens BIGINT NOT NULL DEFAULT 0, context_window BIGINT NOT NULL DEFAULT 0, reasoning_effort TEXT NOT NULL DEFAULT '', model_kind TEXT NOT NULL DEFAULT 'conversation', input_modalities JSONB NOT NULL DEFAULT '["text"]'::jsonb, provider_config JSONB NOT NULL DEFAULT '{}'::jsonb, revision BIGINT NOT NULL CHECK(revision>0), api_key_version BIGINT NOT NULL DEFAULT 1, credential_version BIGINT NOT NULL DEFAULT 0, api_key_key_id TEXT NOT NULL DEFAULT '', api_key_envelope_version BIGINT NOT NULL DEFAULT 0, api_key_aad_version BIGINT NOT NULL DEFAULT 0, api_key_nonce BYTEA NOT NULL DEFAULT ''::bytea, api_key_ciphertext BYTEA NOT NULL DEFAULT ''::bytea, api_key_profile_revision BIGINT NOT NULL DEFAULT 0, deleted_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, PRIMARY KEY(owner_id,profile_id), UNIQUE(owner_id,client_profile_id), CONSTRAINT p2p_agent_model_profiles_api_key_envelope_check CHECK((credential_version=0 AND octet_length(api_key_ciphertext)=0 AND api_key_key_id='' AND octet_length(api_key_nonce)=0 AND api_key_envelope_version=0 AND api_key_aad_version=0) OR (credential_version>0 AND octet_length(api_key_ciphertext)>16 AND octet_length(api_key_nonce)=12 AND api_key_key_id<>'' AND api_key_envelope_version=1 AND api_key_aad_version=1)))`,
		`CREATE TABLE IF NOT EXISTS p2p_agent_model_profile_credentials (owner_id TEXT NOT NULL, profile_id TEXT NOT NULL, credential_version BIGINT NOT NULL CHECK(credential_version>0), profile_revision BIGINT NOT NULL DEFAULT 0, provider TEXT NOT NULL, api_key_key_id TEXT NOT NULL DEFAULT '', api_key_envelope_version BIGINT NOT NULL DEFAULT 0, api_key_aad_version BIGINT NOT NULL DEFAULT 0, api_key_nonce BYTEA NOT NULL, api_key_ciphertext BYTEA NOT NULL, created_at TIMESTAMPTZ NOT NULL, PRIMARY KEY(owner_id,profile_id,credential_version), CONSTRAINT p2p_agent_model_profile_credentials_owner_id_profile_id_fkey FOREIGN KEY(owner_id,profile_id) REFERENCES p2p_agent_model_profiles(owner_id,profile_id) ON DELETE RESTRICT, CONSTRAINT p2p_agent_model_profile_credentials_api_key_envelope_check CHECK(credential_version>0 AND octet_length(api_key_ciphertext)>16 AND octet_length(api_key_nonce)=12 AND api_key_key_id<>'' AND api_key_envelope_version=1 AND api_key_aad_version=1))`,
		`CREATE TABLE IF NOT EXISTS p2p_agent_model_profile_revisions (owner_id TEXT NOT NULL, profile_id TEXT NOT NULL, profile_revision BIGINT NOT NULL CHECK(profile_revision>0), client_profile_id TEXT NOT NULL, display_name TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL, base_url TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', system_prompt TEXT NOT NULL DEFAULT '', temperature DOUBLE PRECISION, top_p DOUBLE PRECISION, max_output_tokens BIGINT NOT NULL DEFAULT 0, context_window BIGINT NOT NULL DEFAULT 0, reasoning_effort TEXT NOT NULL DEFAULT '', model_kind TEXT NOT NULL DEFAULT 'conversation', input_modalities JSONB NOT NULL DEFAULT '["text"]'::jsonb, provider_config JSONB NOT NULL DEFAULT '{}'::jsonb, credential_version BIGINT NOT NULL DEFAULT 0, deleted_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL, PRIMARY KEY(owner_id,profile_id,profile_revision))`,
		`CREATE TABLE IF NOT EXISTS p2p_agent_model_profile_defaults (owner_id TEXT PRIMARY KEY, profile_id TEXT, client_profile_id TEXT NOT NULL, embedding_profile_id TEXT, embedding_client_profile_id TEXT NOT NULL DEFAULT '', speech_profile_id TEXT, speech_client_profile_id TEXT NOT NULL DEFAULT '', CONSTRAINT p2p_agent_model_profile_defaults_owner_profile_fkey FOREIGN KEY(owner_id,profile_id) REFERENCES p2p_agent_model_profiles(owner_id,profile_id) ON DELETE RESTRICT, CONSTRAINT p2p_agent_model_profile_defaults_owner_embedding_fkey FOREIGN KEY(owner_id,embedding_profile_id) REFERENCES p2p_agent_model_profiles(owner_id,profile_id) ON DELETE RESTRICT, CONSTRAINT p2p_agent_model_profile_defaults_owner_speech_fkey FOREIGN KEY(owner_id,speech_profile_id) REFERENCES p2p_agent_model_profiles(owner_id,profile_id) ON DELETE RESTRICT)`,
		`CREATE TABLE IF NOT EXISTS p2p_agent_model_profile_syncs (owner_id TEXT NOT NULL,idempotency_key TEXT NOT NULL,request_digest BYTEA NOT NULL CHECK(octet_length(request_digest)=32),response_json JSONB NOT NULL,created_at TIMESTAMPTZ NOT NULL,PRIMARY KEY(owner_id,idempotency_key))`,
		`CREATE TABLE IF NOT EXISTS p2p_agent_model_profile_deletes (owner_id TEXT NOT NULL,idempotency_key TEXT NOT NULL,profile_id TEXT NOT NULL,request_digest BYTEA,response_json JSONB NOT NULL DEFAULT '{}'::jsonb,created_at TIMESTAMPTZ NOT NULL,PRIMARY KEY(owner_id,idempotency_key))`,
		`CREATE TABLE IF NOT EXISTS p2p_agent_schedule_runs (run_id TEXT PRIMARY KEY NOT NULL,schedule_id TEXT NOT NULL,owner_id TEXT NOT NULL,status TEXT NOT NULL,scheduled_for TIMESTAMPTZ NOT NULL,started_at TIMESTAMPTZ,finished_at TIMESTAMPTZ,result TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '',lease_epoch BIGINT NOT NULL DEFAULT 0,occurrence_id UUID,task_id UUID,created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE UNIQUE INDEX IF NOT EXISTS p2p_agent_schedule_runs_occurrence_idx ON p2p_agent_schedule_runs(owner_id,schedule_id,scheduled_for)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS p2p_agent_schedule_runs_occurrence_link_idx ON p2p_agent_schedule_runs(owner_id,schedule_id,occurrence_id) WHERE occurrence_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS p2p_agent_schedule_runs_task_idx ON p2p_agent_schedule_runs(owner_id,task_id) WHERE task_id IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS p2p_agent_schedule_mutations (owner_id TEXT NOT NULL,action TEXT NOT NULL,idempotency_key TEXT NOT NULL,request_digest BYTEA NOT NULL CHECK(octet_length(request_digest)=32),response_json JSONB NOT NULL,created_at TIMESTAMPTZ NOT NULL,PRIMARY KEY(owner_id,action,idempotency_key))`,
		`CREATE TABLE IF NOT EXISTS p2p_agent_schedule_confirmations (confirmation_id TEXT NOT NULL,owner_id TEXT NOT NULL,conversation_id TEXT NOT NULL,action TEXT NOT NULL,params_json JSONB NOT NULL,request_digest BYTEA NOT NULL CHECK(octet_length(request_digest)=32),idempotency_key TEXT NOT NULL,summary TEXT NOT NULL,approval_code TEXT NOT NULL,status TEXT NOT NULL CHECK(status IN ('pending','executing','completed','failed','expired','replaced')),revision BIGINT NOT NULL DEFAULT 1,expires_at TIMESTAMPTZ NOT NULL,created_at TIMESTAMPTZ NOT NULL,updated_at TIMESTAMPTZ NOT NULL,result_json JSONB NOT NULL DEFAULT '{}'::jsonb,error_text TEXT NOT NULL DEFAULT '',PRIMARY KEY(owner_id,conversation_id,confirmation_id))`,
		`CREATE INDEX IF NOT EXISTS p2p_agent_schedule_confirmations_pending_idx ON p2p_agent_schedule_confirmations(owner_id,conversation_id,status,updated_at)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS p2p_agent_schedule_confirmations_active_idx ON p2p_agent_schedule_confirmations(owner_id,conversation_id) WHERE status IN ('pending','executing')`,
		`CREATE TABLE IF NOT EXISTS p2p_native_agent_memory_turns (owner_id TEXT NOT NULL,conversation_id TEXT NOT NULL,turn_id TEXT NOT NULL,created_at TIMESTAMPTZ NOT NULL,PRIMARY KEY(owner_id,conversation_id,turn_id),FOREIGN KEY(owner_id,conversation_id) REFERENCES p2p_native_agent_conversations(owner_id,conversation_id) ON DELETE CASCADE)`,
		`CREATE TABLE IF NOT EXISTS p2p_native_agent_conversation_mutations (owner_id TEXT NOT NULL,action TEXT NOT NULL,idempotency_key TEXT NOT NULL,request_digest BYTEA NOT NULL CHECK(octet_length(request_digest)=32),response_json JSONB NOT NULL DEFAULT '{}'::jsonb,created_at TIMESTAMPTZ NOT NULL,PRIMARY KEY(owner_id,action,idempotency_key))`,
		`CREATE TABLE IF NOT EXISTS p2p_native_agent_memory_embeddings (owner_id TEXT NOT NULL,memory_id TEXT NOT NULL,profile_id TEXT NOT NULL,profile_revision BIGINT NOT NULL CHECK(profile_revision>0),model TEXT NOT NULL,dimension BIGINT NOT NULL CHECK(dimension BETWEEN 1 AND 32768),content_digest BYTEA NOT NULL CHECK(octet_length(content_digest)=32),vector DOUBLE PRECISION[] NOT NULL CHECK(COALESCE(array_length(vector,1),0)=dimension),indexed_at TIMESTAMPTZ NOT NULL,PRIMARY KEY(owner_id,memory_id),FOREIGN KEY(owner_id,memory_id) REFERENCES p2p_native_agent_memory_records(owner_id,memory_id) ON DELETE CASCADE)`,
		`CREATE INDEX IF NOT EXISTS p2p_native_agent_memory_embeddings_profile_idx ON p2p_native_agent_memory_embeddings(owner_id,profile_id,profile_revision,model,dimension)`,
		`CREATE TABLE IF NOT EXISTS p2p_native_agent_knowledge_sources (owner_id TEXT NOT NULL,source_id TEXT NOT NULL,kind TEXT NOT NULL,status TEXT NOT NULL,title TEXT NOT NULL DEFAULT '',mime_type TEXT NOT NULL,size BIGINT NOT NULL,total_chunks BIGINT NOT NULL DEFAULT 0,indexed_chunks BIGINT NOT NULL DEFAULT 0,revision BIGINT NOT NULL DEFAULT 1,error_text TEXT NOT NULL DEFAULT '',created_at TIMESTAMPTZ NOT NULL,updated_at TIMESTAMPTZ NOT NULL,PRIMARY KEY(owner_id,source_id))`,
		`CREATE INDEX IF NOT EXISTS p2p_native_agent_knowledge_sources_cursor_idx ON p2p_native_agent_knowledge_sources(owner_id,created_at,source_id)`,
		`CREATE TABLE IF NOT EXISTS p2p_native_agent_knowledge_uploads (owner_id TEXT NOT NULL,upload_id TEXT NOT NULL,source_id TEXT NOT NULL,filename TEXT NOT NULL,mime_type TEXT NOT NULL,size BIGINT NOT NULL,received_size BIGINT NOT NULL DEFAULT 0,data BYTEA NOT NULL DEFAULT '',created_at TIMESTAMPTZ NOT NULL,PRIMARY KEY(owner_id,upload_id))`,
		`CREATE TABLE IF NOT EXISTS p2p_agent_secrets (secret_domain TEXT NOT NULL, owner_id TEXT NOT NULL, entity_id TEXT NOT NULL, secret_revision BIGINT NOT NULL CHECK(secret_revision>0), purpose TEXT NOT NULL, reference TEXT NOT NULL, binding_digest BYTEA NOT NULL CHECK(octet_length(binding_digest)=32), envelope_version SMALLINT NOT NULL DEFAULT 1 CHECK(envelope_version=1), aad_version SMALLINT NOT NULL DEFAULT 1 CHECK(aad_version=1), key_id TEXT NOT NULL, nonce BYTEA NOT NULL CHECK(octet_length(nonce)=12), ciphertext BYTEA NOT NULL CHECK(octet_length(ciphertext)>=16), created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(), PRIMARY KEY(secret_domain,owner_id,entity_id,secret_revision,purpose,reference))`,
		`CREATE TABLE IF NOT EXISTS core_execution_secrets (owner_id TEXT NOT NULL CHECK(owner_id<>''),secret_ref UUID NOT NULL,revision BIGINT NOT NULL CHECK(revision>0),purpose TEXT NOT NULL CHECK(purpose='ai_provider_api_key'),provider TEXT NOT NULL CHECK(provider ~ '^[a-z0-9]+([._-][a-z0-9]+)*$' AND char_length(provider)<=64),binding_digest TEXT NOT NULL CHECK(binding_digest ~ '^[a-f0-9]{64}$'),status TEXT NOT NULL CHECK(status IN ('active','revoked')),mutation_kind TEXT NOT NULL CHECK(mutation_kind IN ('create','revoke')),idempotency_key UUID NOT NULL,created_at TIMESTAMPTZ NOT NULL,updated_at TIMESTAMPTZ NOT NULL,PRIMARY KEY(owner_id,secret_ref,revision),UNIQUE(owner_id,idempotency_key),CHECK((revision=1 AND status='active' AND mutation_kind='create') OR (revision>1 AND status='revoked' AND mutation_kind='revoke')))`,
		`CREATE INDEX IF NOT EXISTS core_execution_secrets_current_idx ON core_execution_secrets(owner_id,secret_ref,revision DESC)`,
		`CREATE OR REPLACE FUNCTION core_execution_secret_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'execution secret metadata is append-only'; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_secrets_immutable ON core_execution_secrets`, `CREATE TRIGGER core_execution_secrets_immutable BEFORE UPDATE OR DELETE ON core_execution_secrets FOR EACH ROW EXECUTE FUNCTION core_execution_secret_immutable()`,
		`CREATE TABLE IF NOT EXISTS p2p_agent_secret_key_usage (secret_domain TEXT NOT NULL, owner_id TEXT NOT NULL, entity_id TEXT NOT NULL, secret_revision BIGINT NOT NULL CHECK(secret_revision>0), purpose TEXT NOT NULL, reference TEXT NOT NULL, key_id TEXT NOT NULL, envelope_digest BYTEA NOT NULL CHECK(octet_length(envelope_digest)=32), updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(), PRIMARY KEY(secret_domain,owner_id,entity_id,secret_revision,purpose,reference))`,
		`CREATE TABLE IF NOT EXISTS p2p_agent_secret_rotations (rotation_id UUID PRIMARY KEY,state TEXT NOT NULL CHECK(state IN ('rewrapping','verifying','complete','failed')),from_key_ids TEXT[] NOT NULL,to_key_id TEXT NOT NULL,lease_owner TEXT NOT NULL DEFAULT '',lease_epoch BIGINT NOT NULL DEFAULT 0,lease_expires_at TIMESTAMPTZ,cursor_domain TEXT NOT NULL DEFAULT '',cursor_owner_id TEXT NOT NULL DEFAULT '',cursor_entity_id TEXT NOT NULL DEFAULT '',cursor_revision BIGINT NOT NULL DEFAULT 0,rewrapped_rows BIGINT NOT NULL DEFAULT 0,verified_rows BIGINT NOT NULL DEFAULT 0,error_code TEXT NOT NULL DEFAULT '',created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),completed_at TIMESTAMPTZ)`,
		`CREATE TABLE IF NOT EXISTS core_aws_credentials (owner_id TEXT NOT NULL,credential_id UUID NOT NULL,revision BIGINT NOT NULL CHECK(revision>0),envelope_version INTEGER NOT NULL,aad_version INTEGER NOT NULL,key_id TEXT,nonce BYTEA,ciphertext BYTEA,envelope_digest TEXT NOT NULL CHECK(envelope_digest ~ '^[a-f0-9]{64}$'),name TEXT NOT NULL,region TEXT NOT NULL,account_id TEXT NOT NULL DEFAULT '',user_arn TEXT NOT NULL DEFAULT '',verified_revision BIGINT NOT NULL DEFAULT 0,created_at TIMESTAMPTZ NOT NULL,updated_at TIMESTAMPTZ NOT NULL,PRIMARY KEY(owner_id,credential_id,revision))`,
		`CREATE TABLE IF NOT EXISTS core_aws_credential_current (owner_id TEXT NOT NULL,credential_id UUID NOT NULL,revision BIGINT NOT NULL CHECK(revision>0),deleted_at TIMESTAMPTZ,PRIMARY KEY(owner_id,credential_id),FOREIGN KEY(owner_id,credential_id,revision) REFERENCES core_aws_credentials(owner_id,credential_id,revision) ON DELETE RESTRICT)`,
		`CREATE TABLE IF NOT EXISTS core_aws_replays (owner_id TEXT NOT NULL,operation TEXT NOT NULL,idempotency_key UUID NOT NULL,request_hash TEXT NOT NULL CHECK(request_hash ~ '^[a-f0-9]{64}$'),response_json JSONB NOT NULL,created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),PRIMARY KEY(owner_id,operation,idempotency_key))`,
		`CREATE INDEX IF NOT EXISTS core_aws_replays_owner_created_idx ON core_aws_replays(owner_id,created_at DESC,operation,idempotency_key)`,
		`CREATE TABLE IF NOT EXISTS core_execution_deployments (owner_id TEXT NOT NULL,deployment_id UUID NOT NULL,project_id UUID NOT NULL,current_run_id UUID NOT NULL,current_stage_id UUID,release_id TEXT,state TEXT NOT NULL DEFAULT 'pending',revision BIGINT NOT NULL DEFAULT 1 CHECK(revision>0),object_json JSONB NOT NULL DEFAULT '{}'::jsonb,actual_json JSONB NOT NULL DEFAULT '{}'::jsonb,created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),PRIMARY KEY(owner_id,deployment_id))`,
		`CREATE TABLE IF NOT EXISTS core_execution_deployment_counters (owner_id TEXT NOT NULL,deployment_id UUID NOT NULL,next_sequence BIGINT NOT NULL DEFAULT 1 CHECK(next_sequence>0),PRIMARY KEY(owner_id,deployment_id),FOREIGN KEY(owner_id,deployment_id) REFERENCES core_execution_deployments(owner_id,deployment_id) ON DELETE RESTRICT)`,
		`CREATE TABLE IF NOT EXISTS core_execution_deployment_events (owner_id TEXT NOT NULL,deployment_id UUID NOT NULL,event_id UUID NOT NULL,sequence BIGINT NOT NULL CHECK(sequence>0),event_digest TEXT NOT NULL CHECK(event_digest ~ '^[a-f0-9]{64}$'),event_json JSONB NOT NULL DEFAULT '{}'::jsonb,created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),PRIMARY KEY(owner_id,deployment_id,sequence),UNIQUE(owner_id,event_id),FOREIGN KEY(owner_id,deployment_id) REFERENCES core_execution_deployments(owner_id,deployment_id) ON DELETE RESTRICT)`,
	}); err != nil {
		return err
	}
	if err := execMigrationStatements(ctx, txn, []string{
		`CREATE TABLE IF NOT EXISTS core_execution_projects (owner_id TEXT NOT NULL, project_id UUID NOT NULL, revision BIGINT NOT NULL DEFAULT 1 CHECK (revision>0), status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','archived')), schema_version TEXT NOT NULL DEFAULT 'execution-project/v2' CHECK (schema_version='execution-project/v2'), project_digest TEXT NOT NULL CHECK (project_digest ~ '^[a-f0-9]{64}$'), snapshot_json JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(owner_id,project_id))`,
		`CREATE TABLE IF NOT EXISTS core_execution_source_artifacts (owner_id TEXT NOT NULL CHECK(owner_id<>''), artifact_id UUID NOT NULL, project_id UUID NOT NULL, content_digest TEXT NOT NULL CHECK(content_digest ~ '^[a-f0-9]{64}$'), storage_backend TEXT NOT NULL DEFAULT 'filesystem' CHECK(storage_backend='filesystem'), storage_ref TEXT NOT NULL CHECK(storage_ref ~ '^sha256/[a-f0-9]{2}/[a-f0-9]{64}$'), size_bytes BIGINT NOT NULL CHECK(size_bytes BETWEEN 1 AND 1073741824), media_type TEXT NOT NULL CHECK(char_length(media_type) BETWEEN 3 AND 255 AND media_type ~ '^[a-z0-9]([a-z0-9._+*-]*[a-z0-9])?/[a-z0-9]([a-z0-9._+*-]*[a-z0-9])?$'), revision BIGINT NOT NULL DEFAULT 1 CHECK(revision=1), status TEXT NOT NULL DEFAULT 'available' CHECK(status='available'), schema_version TEXT NOT NULL DEFAULT 'execution-source-artifact/v2' CHECK(schema_version='execution-source-artifact/v2'), metadata_json JSONB NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(metadata_json)='object'), created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(owner_id,artifact_id), CHECK(right(storage_ref,64)=content_digest AND substring(storage_ref FROM 8 FOR 2)=left(content_digest,2)), FOREIGN KEY(owner_id,project_id) REFERENCES core_execution_projects(owner_id,project_id) ON DELETE RESTRICT)`,
		`CREATE INDEX IF NOT EXISTS core_execution_source_artifacts_project_idx ON core_execution_source_artifacts(owner_id,project_id,created_at,artifact_id)`,
		`CREATE INDEX IF NOT EXISTS core_execution_source_artifacts_content_idx ON core_execution_source_artifacts(owner_id,content_digest)`,
		`CREATE OR REPLACE FUNCTION core_execution_source_artifact_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'execution source artifacts are immutable'; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_source_artifacts_immutable ON core_execution_source_artifacts`, `CREATE TRIGGER core_execution_source_artifacts_immutable BEFORE UPDATE OR DELETE ON core_execution_source_artifacts FOR EACH ROW EXECUTE FUNCTION core_execution_source_artifact_immutable()`,
		`CREATE TABLE IF NOT EXISTS core_execution_analyses (owner_id TEXT NOT NULL, analysis_id UUID NOT NULL, project_id UUID NOT NULL, revision BIGINT NOT NULL DEFAULT 1 CHECK (revision>0), status TEXT NOT NULL DEFAULT 'ready' CHECK (status IN ('pending','ready','completed','failed','superseded')), schema_version TEXT NOT NULL DEFAULT 'execution-analysis/v2' CHECK (schema_version='execution-analysis/v2'), analysis_digest TEXT NOT NULL CHECK (analysis_digest ~ '^[a-f0-9]{64}$'), snapshot_json JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(owner_id,analysis_id), UNIQUE(owner_id,project_id,analysis_id), FOREIGN KEY(owner_id,project_id) REFERENCES core_execution_projects(owner_id,project_id) ON DELETE RESTRICT)`,
		`CREATE INDEX IF NOT EXISTS core_execution_projects_owner_status_idx ON core_execution_projects(owner_id,status,updated_at DESC,project_id)`,
		`CREATE INDEX IF NOT EXISTS core_execution_analyses_project_idx ON core_execution_analyses(owner_id,project_id,created_at DESC,analysis_id)`,
		`CREATE OR REPLACE FUNCTION core_execution_analysis_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'execution analysis snapshots are immutable'; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_analyses_immutable ON core_execution_analyses`, `CREATE TRIGGER core_execution_analyses_immutable BEFORE UPDATE OR DELETE ON core_execution_analyses FOR EACH ROW EXECUTE FUNCTION core_execution_analysis_immutable()`,
		`CREATE TABLE IF NOT EXISTS core_execution_targets (owner_id TEXT NOT NULL, target_id UUID NOT NULL, target_revision BIGINT NOT NULL DEFAULT 1 CHECK (target_revision>0), status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','archived')), schema_version TEXT NOT NULL DEFAULT 'execution-target/v2' CHECK (schema_version='execution-target/v2'), provider TEXT NOT NULL DEFAULT '', target_digest TEXT NOT NULL CHECK (target_digest ~ '^[a-f0-9]{64}$'), snapshot_json JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(owner_id,target_id,target_revision), UNIQUE(owner_id,target_id,target_revision,target_digest))`,
		`CREATE OR REPLACE FUNCTION core_execution_target_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'execution target snapshots are immutable'; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_targets_immutable ON core_execution_targets`, `CREATE TRIGGER core_execution_targets_immutable BEFORE UPDATE OR DELETE ON core_execution_targets FOR EACH ROW EXECUTE FUNCTION core_execution_target_immutable()`,
		`CREATE INDEX IF NOT EXISTS core_execution_targets_owner_status_provider_idx ON core_execution_targets(owner_id,status,provider,target_id,target_revision DESC)`,
		`CREATE TABLE IF NOT EXISTS core_execution_target_observations (owner_id TEXT NOT NULL, observation_id UUID NOT NULL, target_id UUID NOT NULL, target_revision BIGINT NOT NULL CHECK (target_revision>0), revision BIGINT NOT NULL DEFAULT 1 CHECK (revision>0), status TEXT NOT NULL DEFAULT 'observed' CHECK (status IN ('observed','failed')), schema_version TEXT NOT NULL DEFAULT 'execution-observation/v2' CHECK (schema_version='execution-observation/v2'), observation_digest TEXT NOT NULL CHECK (observation_digest ~ '^[a-f0-9]{64}$'), snapshot_json JSONB NOT NULL DEFAULT '{}'::jsonb, observed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(owner_id,observation_id), FOREIGN KEY(owner_id,target_id,target_revision) REFERENCES core_execution_targets(owner_id,target_id,target_revision) ON DELETE RESTRICT)`,
		`CREATE INDEX IF NOT EXISTS core_execution_target_observations_target_idx ON core_execution_target_observations(owner_id,target_id,target_revision,observed_at DESC,observation_id)`,
		`CREATE OR REPLACE FUNCTION core_execution_target_observation_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF TG_OP='DELETE' OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.observation_id IS DISTINCT FROM OLD.observation_id OR NEW.target_id IS DISTINCT FROM OLD.target_id OR NEW.target_revision IS DISTINCT FROM OLD.target_revision OR NEW.revision IS DISTINCT FROM OLD.revision OR NEW.status IS DISTINCT FROM OLD.status OR NEW.schema_version IS DISTINCT FROM OLD.schema_version OR NEW.observation_digest IS DISTINCT FROM OLD.observation_digest OR NEW.snapshot_json IS DISTINCT FROM OLD.snapshot_json OR NEW.observed_at IS DISTINCT FROM OLD.observed_at THEN RAISE EXCEPTION 'execution target observations are immutable'; END IF; RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_target_observations_immutable ON core_execution_target_observations`, `CREATE TRIGGER core_execution_target_observations_immutable BEFORE UPDATE OR DELETE ON core_execution_target_observations FOR EACH ROW EXECUTE FUNCTION core_execution_target_observation_immutable()`,
		`CREATE TABLE IF NOT EXISTS core_execution_skill_versions (owner_id TEXT NOT NULL, id TEXT NOT NULL CHECK (id ~ '^[a-z0-9][a-z0-9._-]*$'), version TEXT NOT NULL CHECK (version ~ '^[0-9]+\.[0-9]+\.[0-9]+([-.+][0-9A-Za-z.-]+)?$'), revision BIGINT NOT NULL DEFAULT 1 CHECK (revision>0), status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','ready','deprecated','revoked')), schema_version TEXT NOT NULL DEFAULT 'execution-skill/v2' CHECK (schema_version='execution-skill/v2'), content_digest TEXT NOT NULL CHECK (content_digest ~ '^[a-f0-9]{64}$'), snapshot_json JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(owner_id,id,version))`,
		`CREATE INDEX IF NOT EXISTS core_execution_skill_versions_owner_id_idx ON core_execution_skill_versions(owner_id,id,version)`,
		`DROP INDEX IF EXISTS core_execution_skill_versions_owner_digest_uidx`, `CREATE INDEX IF NOT EXISTS core_execution_skill_versions_owner_digest_idx ON core_execution_skill_versions(owner_id,content_digest)`,
		`CREATE TABLE IF NOT EXISTS core_execution_recipe_versions (owner_id TEXT NOT NULL, id TEXT NOT NULL CHECK (id ~ '^[a-z0-9][a-z0-9._-]*$'), version TEXT NOT NULL CHECK (version ~ '^[0-9]+\.[0-9]+\.[0-9]+([-.+][0-9A-Za-z.-]+)?$'), revision BIGINT NOT NULL DEFAULT 1 CHECK (revision>0), status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','ready','deprecated','revoked')), schema_version TEXT NOT NULL DEFAULT 'execution-recipe/v2' CHECK (schema_version='execution-recipe/v2'), content_digest TEXT NOT NULL CHECK (content_digest ~ '^[a-f0-9]{64}$'), snapshot_json JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(owner_id,id,version))`,
		`CREATE INDEX IF NOT EXISTS core_execution_recipe_versions_owner_id_idx ON core_execution_recipe_versions(owner_id,id,version)`,
		`DROP INDEX IF EXISTS core_execution_recipe_versions_owner_digest_uidx`, `CREATE INDEX IF NOT EXISTS core_execution_recipe_versions_owner_digest_idx ON core_execution_recipe_versions(owner_id,content_digest)`,
		`CREATE OR REPLACE FUNCTION core_execution_version_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF TG_OP='DELETE' OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.id IS DISTINCT FROM OLD.id OR NEW.version IS DISTINCT FROM OLD.version OR NEW.revision IS DISTINCT FROM OLD.revision OR NEW.schema_version IS DISTINCT FROM OLD.schema_version OR NEW.content_digest IS DISTINCT FROM OLD.content_digest OR NEW.snapshot_json IS DISTINCT FROM OLD.snapshot_json THEN RAISE EXCEPTION 'execution skill/recipe version is immutable'; END IF; RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_skill_versions_immutable ON core_execution_skill_versions`, `CREATE TRIGGER core_execution_skill_versions_immutable BEFORE UPDATE OR DELETE ON core_execution_skill_versions FOR EACH ROW EXECUTE FUNCTION core_execution_version_immutable()`,
		`DROP TRIGGER IF EXISTS core_execution_recipe_versions_immutable ON core_execution_recipe_versions`, `CREATE TRIGGER core_execution_recipe_versions_immutable BEFORE UPDATE OR DELETE ON core_execution_recipe_versions FOR EACH ROW EXECUTE FUNCTION core_execution_version_immutable()`,
		`CREATE TABLE IF NOT EXISTS core_execution_plans (owner_id TEXT NOT NULL, plan_id UUID NOT NULL, project_id UUID NOT NULL, revision BIGINT NOT NULL DEFAULT 1 CHECK (revision>0), status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','ready','expired','superseded')), schema_version TEXT NOT NULL DEFAULT 'execution-plan/v2' CHECK (schema_version='execution-plan/v2'), snapshot_json JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(owner_id,plan_id), FOREIGN KEY(owner_id,project_id) REFERENCES core_execution_projects(owner_id,project_id) ON DELETE RESTRICT)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS core_execution_plans_project_pin_uidx ON core_execution_plans(owner_id,project_id,plan_id)`,
		`CREATE INDEX IF NOT EXISTS core_execution_plans_project_status_idx ON core_execution_plans(owner_id,project_id,status,updated_at DESC,plan_id)`,
		`CREATE OR REPLACE FUNCTION core_execution_plan_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF TG_OP='DELETE' OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.plan_id IS DISTINCT FROM OLD.plan_id OR NEW.project_id IS DISTINCT FROM OLD.project_id OR NEW.revision <= OLD.revision OR NEW.schema_version IS DISTINCT FROM OLD.schema_version OR NEW.snapshot_json IS DISTINCT FROM OLD.snapshot_json OR (OLD.status IN ('ready','expired','superseded') AND NEW.status='draft') THEN RAISE EXCEPTION 'execution plan identity/state is immutable or non-monotonic'; END IF; RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_plans_immutable ON core_execution_plans`, `CREATE TRIGGER core_execution_plans_immutable BEFORE UPDATE OR DELETE ON core_execution_plans FOR EACH ROW EXECUTE FUNCTION core_execution_plan_immutable()`,
		`CREATE TABLE IF NOT EXISTS core_execution_plan_revisions (owner_id TEXT NOT NULL, plan_id UUID NOT NULL, plan_revision_id UUID NOT NULL, revision BIGINT NOT NULL CHECK (revision>0), project_id UUID NOT NULL, analysis_id UUID NOT NULL, status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','ready','expired','superseded')), schema_version TEXT NOT NULL DEFAULT 'execution-plan/v2' CHECK (schema_version='execution-plan/v2'), plan_digest TEXT NOT NULL CHECK (plan_digest ~ '^[a-f0-9]{64}$'), snapshot_json JSONB NOT NULL CHECK(jsonb_typeof(snapshot_json)='object' AND snapshot_json<>'{}'::jsonb), expires_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(owner_id,plan_id,revision), UNIQUE(owner_id,plan_revision_id), FOREIGN KEY(owner_id,project_id,plan_id) REFERENCES core_execution_plans(owner_id,project_id,plan_id) ON DELETE RESTRICT, FOREIGN KEY(owner_id,project_id,analysis_id) REFERENCES core_execution_analyses(owner_id,project_id,analysis_id) ON DELETE RESTRICT)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS core_execution_plan_revisions_pin_uidx ON core_execution_plan_revisions(owner_id,project_id,plan_id,revision,plan_digest)`,
		`CREATE INDEX IF NOT EXISTS core_execution_plan_revisions_plan_idx ON core_execution_plan_revisions(owner_id,plan_id,revision DESC)`,
		`CREATE TABLE IF NOT EXISTS core_execution_plan_stages (owner_id TEXT NOT NULL, plan_id UUID NOT NULL, plan_revision BIGINT NOT NULL, stage_key TEXT NOT NULL CHECK(stage_key<>''), stage_revision BIGINT NOT NULL DEFAULT 1 CHECK(stage_revision>0), stage_digest TEXT NOT NULL CHECK(stage_digest ~ '^[a-f0-9]{64}$'), ordinal BIGINT NOT NULL CHECK(ordinal>=1), status TEXT NOT NULL DEFAULT 'ready' CHECK(status IN ('draft','ready','expired','superseded')), schema_version TEXT NOT NULL DEFAULT 'execution-plan/v2' CHECK(schema_version='execution-plan/v2'), snapshot_json JSONB NOT NULL DEFAULT '{}'::jsonb, PRIMARY KEY(owner_id,plan_id,plan_revision,stage_key), UNIQUE(owner_id,plan_id,plan_revision,ordinal), FOREIGN KEY(owner_id,plan_id,plan_revision) REFERENCES core_execution_plan_revisions(owner_id,plan_id,revision) ON DELETE RESTRICT)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS core_execution_plan_stages_pin_uidx ON core_execution_plan_stages(owner_id,plan_id,plan_revision,stage_key,stage_revision,stage_digest)`,
		`CREATE INDEX IF NOT EXISTS core_execution_plan_stages_order_idx ON core_execution_plan_stages(owner_id,plan_id,plan_revision,ordinal,stage_key)`,
		`CREATE TABLE IF NOT EXISTS core_execution_plan_steps (owner_id TEXT NOT NULL, plan_id UUID NOT NULL, plan_revision BIGINT NOT NULL, stage_key TEXT NOT NULL CHECK(stage_key<>''), step_key TEXT NOT NULL CHECK(step_key<>''), step_set TEXT NOT NULL DEFAULT 'forward' CHECK(step_set IN ('forward','rollback')), step_revision BIGINT NOT NULL DEFAULT 1 CHECK(step_revision>0), step_digest TEXT NOT NULL CHECK(step_digest ~ '^[a-f0-9]{64}$'), ordinal BIGINT NOT NULL CHECK(ordinal>=1), status TEXT NOT NULL DEFAULT 'ready' CHECK(status IN ('draft','ready','expired','superseded')), schema_version TEXT NOT NULL DEFAULT 'execution-plan/v2' CHECK(schema_version='execution-plan/v2'), snapshot_json JSONB NOT NULL CHECK(jsonb_typeof(snapshot_json)='object' AND snapshot_json<>'{}'::jsonb), PRIMARY KEY(owner_id,plan_id,plan_revision,stage_key,step_set,step_key), UNIQUE(owner_id,plan_id,plan_revision,stage_key,step_set,ordinal), FOREIGN KEY(owner_id,plan_id,plan_revision,stage_key) REFERENCES core_execution_plan_stages(owner_id,plan_id,plan_revision,stage_key) ON DELETE RESTRICT)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS core_execution_plan_steps_pin_uidx ON core_execution_plan_steps(owner_id,plan_id,plan_revision,stage_key,step_set,step_key,step_revision,step_digest)`,
		`CREATE OR REPLACE FUNCTION core_execution_plan_snapshot_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF TG_OP='DELETE' OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.project_id IS DISTINCT FROM OLD.project_id OR NEW.plan_id IS DISTINCT FROM OLD.plan_id OR NEW.plan_revision_id IS DISTINCT FROM OLD.plan_revision_id OR NEW.revision IS DISTINCT FROM OLD.revision OR NEW.analysis_id IS DISTINCT FROM OLD.analysis_id OR NEW.schema_version IS DISTINCT FROM OLD.schema_version OR NEW.plan_digest IS DISTINCT FROM OLD.plan_digest OR NEW.snapshot_json IS DISTINCT FROM OLD.snapshot_json OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN RAISE EXCEPTION 'execution plan snapshot is immutable'; END IF; RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_plan_revisions_immutable ON core_execution_plan_revisions`, `CREATE TRIGGER core_execution_plan_revisions_immutable BEFORE UPDATE OR DELETE ON core_execution_plan_revisions FOR EACH ROW EXECUTE FUNCTION core_execution_plan_snapshot_immutable()`,
		`CREATE OR REPLACE FUNCTION core_execution_plan_stage_snapshot_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF TG_OP='DELETE' OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.plan_id IS DISTINCT FROM OLD.plan_id OR NEW.plan_revision IS DISTINCT FROM OLD.plan_revision OR NEW.stage_key IS DISTINCT FROM OLD.stage_key OR NEW.stage_revision IS DISTINCT FROM OLD.stage_revision OR NEW.stage_digest IS DISTINCT FROM OLD.stage_digest OR NEW.ordinal IS DISTINCT FROM OLD.ordinal OR NEW.schema_version IS DISTINCT FROM OLD.schema_version OR NEW.snapshot_json IS DISTINCT FROM OLD.snapshot_json THEN RAISE EXCEPTION 'execution plan stage snapshot is immutable'; END IF; RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_plan_stages_immutable ON core_execution_plan_stages`, `CREATE TRIGGER core_execution_plan_stages_immutable BEFORE UPDATE OR DELETE ON core_execution_plan_stages FOR EACH ROW EXECUTE FUNCTION core_execution_plan_stage_snapshot_immutable()`,
		`CREATE OR REPLACE FUNCTION core_execution_plan_step_snapshot_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF TG_OP='DELETE' OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.plan_id IS DISTINCT FROM OLD.plan_id OR NEW.plan_revision IS DISTINCT FROM OLD.plan_revision OR NEW.stage_key IS DISTINCT FROM OLD.stage_key OR NEW.step_key IS DISTINCT FROM OLD.step_key OR NEW.step_set IS DISTINCT FROM OLD.step_set OR NEW.step_revision IS DISTINCT FROM OLD.step_revision OR NEW.step_digest IS DISTINCT FROM OLD.step_digest OR NEW.ordinal IS DISTINCT FROM OLD.ordinal OR NEW.schema_version IS DISTINCT FROM OLD.schema_version OR NEW.snapshot_json IS DISTINCT FROM OLD.snapshot_json THEN RAISE EXCEPTION 'execution plan step snapshot is immutable'; END IF; RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_plan_steps_immutable ON core_execution_plan_steps`, `CREATE TRIGGER core_execution_plan_steps_immutable BEFORE UPDATE OR DELETE ON core_execution_plan_steps FOR EACH ROW EXECUTE FUNCTION core_execution_plan_step_snapshot_immutable()`,
		`CREATE INDEX IF NOT EXISTS core_execution_plan_steps_order_idx ON core_execution_plan_steps(owner_id,plan_id,plan_revision,stage_key,ordinal,step_key)`,
	}); err != nil {
		return err
	}
	if err := execMigrationStatements(ctx, txn, []string{
		`CREATE TABLE IF NOT EXISTS core_execution_runs (owner_id TEXT NOT NULL, run_id UUID NOT NULL, project_id UUID NOT NULL, plan_id UUID NOT NULL, plan_revision BIGINT NOT NULL CHECK(plan_revision>0), rollback_of_run_id UUID, deployment_id UUID, operation TEXT NOT NULL DEFAULT 'execute' CHECK(operation IN ('deploy','upgrade','repair','destroy','rollback','execute')), purpose TEXT NOT NULL DEFAULT 'job' CHECK(purpose IN ('job','service')), trigger_kind TEXT NOT NULL DEFAULT 'manual' CHECK(trigger_kind IN ('manual','schedule','retry','reconcile','rollback')), plan_digest TEXT NOT NULL CHECK(plan_digest ~ '^[a-f0-9]{64}$'), current_stage TEXT NOT NULL DEFAULT '', current_stage_id UUID, revision BIGINT NOT NULL DEFAULT 1 CHECK(revision>0), status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','waiting_user','queued','running','succeeded','failed','uncertain','canceled','rejected','expired')), terminal_reason TEXT NOT NULL DEFAULT '', started_at TIMESTAMPTZ, completed_at TIMESTAMPTZ, schema_version TEXT NOT NULL DEFAULT 'execution-run/v2' CHECK(schema_version='execution-run/v2'), run_digest TEXT NOT NULL CHECK(run_digest ~ '^[a-f0-9]{64}$'), snapshot_json JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(owner_id,run_id), CHECK((operation='rollback')=(rollback_of_run_id IS NOT NULL)), FOREIGN KEY(owner_id,rollback_of_run_id) REFERENCES core_execution_runs(owner_id,run_id) ON DELETE RESTRICT, UNIQUE(owner_id,run_id,project_id), UNIQUE(owner_id,run_id,project_id,plan_id,plan_revision), UNIQUE(owner_id,project_id,plan_id,plan_revision,run_id), UNIQUE(owner_id,run_id,revision), FOREIGN KEY(owner_id,project_id) REFERENCES core_execution_projects(owner_id,project_id) ON DELETE RESTRICT, FOREIGN KEY(owner_id,project_id,plan_id,plan_revision,plan_digest) REFERENCES core_execution_plan_revisions(owner_id,project_id,plan_id,revision,plan_digest) ON DELETE RESTRICT)`,
		`CREATE INDEX IF NOT EXISTS core_execution_runs_project_status_idx ON core_execution_runs(owner_id,project_id,status,updated_at DESC,run_id)`,
		`CREATE TABLE IF NOT EXISTS core_execution_run_revisions (owner_id TEXT NOT NULL, run_id UUID NOT NULL, revision BIGINT NOT NULL CHECK(revision>0), project_id UUID NOT NULL, plan_id UUID NOT NULL, plan_revision BIGINT NOT NULL CHECK(plan_revision>0), rollback_of_run_id UUID, deployment_id UUID, operation TEXT NOT NULL CHECK(operation IN ('deploy','upgrade','repair','destroy','rollback','execute')), purpose TEXT NOT NULL CHECK(purpose IN ('job','service')), trigger_kind TEXT NOT NULL CHECK(trigger_kind IN ('manual','schedule','retry','reconcile','rollback')), plan_digest TEXT NOT NULL CHECK(plan_digest ~ '^[a-f0-9]{64}$'), current_stage TEXT NOT NULL DEFAULT '', current_stage_id UUID, status TEXT NOT NULL CHECK(status IN ('pending','waiting_user','queued','running','succeeded','failed','uncertain','canceled','rejected','expired')), terminal_reason TEXT NOT NULL DEFAULT '', started_at TIMESTAMPTZ, completed_at TIMESTAMPTZ, schema_version TEXT NOT NULL CHECK(schema_version='execution-run/v2'), run_digest TEXT NOT NULL CHECK(run_digest ~ '^[a-f0-9]{64}$'), snapshot_json JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, PRIMARY KEY(owner_id,run_id,revision), CHECK((operation='rollback')=(rollback_of_run_id IS NOT NULL)), FOREIGN KEY(owner_id,run_id) REFERENCES core_execution_runs(owner_id,run_id) ON DELETE RESTRICT, FOREIGN KEY(owner_id,project_id) REFERENCES core_execution_projects(owner_id,project_id) ON DELETE RESTRICT, FOREIGN KEY(owner_id,project_id,plan_id,plan_revision,plan_digest) REFERENCES core_execution_plan_revisions(owner_id,project_id,plan_id,revision,plan_digest) ON DELETE RESTRICT)`,
		`CREATE INDEX IF NOT EXISTS core_execution_run_revisions_owner_run_idx ON core_execution_run_revisions(owner_id,run_id,revision)`,
		`CREATE OR REPLACE FUNCTION core_execution_run_revision_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'execution run revisions are append-only'; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_run_revisions_immutable ON core_execution_run_revisions`, `CREATE TRIGGER core_execution_run_revisions_immutable BEFORE UPDATE OR DELETE ON core_execution_run_revisions FOR EACH ROW EXECUTE FUNCTION core_execution_run_revision_immutable()`,
		`CREATE OR REPLACE FUNCTION core_execution_run_revision_insert_guard() RETURNS trigger LANGUAGE plpgsql AS $$ DECLARE current_run core_execution_runs%ROWTYPE; BEGIN SELECT * INTO current_run FROM core_execution_runs WHERE owner_id=NEW.owner_id AND run_id=NEW.run_id FOR KEY SHARE; IF NOT FOUND THEN RAISE EXCEPTION 'execution run revision identity mismatch'; END IF; IF NEW.revision<>current_run.revision THEN RAISE EXCEPTION 'execution run revision is future or non-current'; END IF; IF NEW.project_id IS DISTINCT FROM current_run.project_id OR NEW.plan_id IS DISTINCT FROM current_run.plan_id OR NEW.plan_revision IS DISTINCT FROM current_run.plan_revision OR NEW.rollback_of_run_id IS DISTINCT FROM current_run.rollback_of_run_id OR NEW.deployment_id IS DISTINCT FROM current_run.deployment_id OR NEW.operation IS DISTINCT FROM current_run.operation OR NEW.purpose IS DISTINCT FROM current_run.purpose OR NEW.trigger_kind IS DISTINCT FROM current_run.trigger_kind OR NEW.plan_digest IS DISTINCT FROM current_run.plan_digest OR NEW.current_stage IS DISTINCT FROM current_run.current_stage OR NEW.current_stage_id IS DISTINCT FROM current_run.current_stage_id OR NEW.status IS DISTINCT FROM current_run.status OR NEW.terminal_reason IS DISTINCT FROM current_run.terminal_reason OR NEW.started_at IS DISTINCT FROM current_run.started_at OR NEW.completed_at IS DISTINCT FROM current_run.completed_at OR NEW.schema_version IS DISTINCT FROM current_run.schema_version OR NEW.run_digest IS DISTINCT FROM current_run.run_digest OR NEW.snapshot_json IS DISTINCT FROM current_run.snapshot_json OR NEW.created_at IS DISTINCT FROM current_run.created_at OR NEW.updated_at IS DISTINCT FROM current_run.updated_at THEN RAISE EXCEPTION 'execution run revision identity mismatch'; END IF; IF NEW.revision>1 AND NOT EXISTS (SELECT 1 FROM core_execution_run_revisions WHERE owner_id=NEW.owner_id AND run_id=NEW.run_id AND revision=NEW.revision-1) THEN RAISE EXCEPTION 'execution run revision must be consecutive'; END IF; RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_run_revisions_insert_guard ON core_execution_run_revisions`, `CREATE TRIGGER core_execution_run_revisions_insert_guard BEFORE INSERT ON core_execution_run_revisions FOR EACH ROW EXECUTE FUNCTION core_execution_run_revision_insert_guard()`,
		`CREATE OR REPLACE FUNCTION core_execution_run_revision_completion_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NOT EXISTS (SELECT 1 FROM core_execution_run_revisions WHERE owner_id=NEW.owner_id AND run_id=NEW.run_id AND revision=NEW.revision) THEN RAISE EXCEPTION 'execution run current revision requires matching history'; END IF; RETURN NULL; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_runs_revision_completion_guard ON core_execution_runs`, `CREATE CONSTRAINT TRIGGER core_execution_runs_revision_completion_guard AFTER INSERT OR UPDATE OF revision ON core_execution_runs DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION core_execution_run_revision_completion_guard()`,
		`CREATE OR REPLACE FUNCTION core_execution_run_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF TG_OP='DELETE' OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.project_id IS DISTINCT FROM OLD.project_id OR NEW.plan_id IS DISTINCT FROM OLD.plan_id OR NEW.plan_revision IS DISTINCT FROM OLD.plan_revision OR NEW.rollback_of_run_id IS DISTINCT FROM OLD.rollback_of_run_id OR NEW.deployment_id IS DISTINCT FROM OLD.deployment_id OR NEW.operation IS DISTINCT FROM OLD.operation OR NEW.purpose IS DISTINCT FROM OLD.purpose OR NEW.trigger_kind IS DISTINCT FROM OLD.trigger_kind OR NEW.plan_digest IS DISTINCT FROM OLD.plan_digest OR NEW.schema_version IS DISTINCT FROM OLD.schema_version OR NEW.run_digest IS DISTINCT FROM OLD.run_digest OR NEW.created_at IS DISTINCT FROM OLD.created_at OR NEW.revision<>OLD.revision+1 OR (OLD.started_at IS NOT NULL AND NEW.started_at IS DISTINCT FROM OLD.started_at) OR (OLD.completed_at IS NOT NULL AND NEW.completed_at IS DISTINCT FROM OLD.completed_at) OR (OLD.terminal_reason<>'' AND NEW.terminal_reason IS DISTINCT FROM OLD.terminal_reason) OR (OLD.status IN ('running','succeeded','failed','uncertain','canceled','rejected','expired') AND NEW.status IN ('pending','waiting_user','queued')) OR (OLD.status IN ('succeeded','failed','uncertain','canceled','rejected','expired') AND NEW.status IS DISTINCT FROM OLD.status) THEN RAISE EXCEPTION 'execution run identity/lifecycle is immutable or revision is not consecutive'; END IF; RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_runs_immutable ON core_execution_runs`, `CREATE TRIGGER core_execution_runs_immutable BEFORE UPDATE OR DELETE ON core_execution_runs FOR EACH ROW EXECUTE FUNCTION core_execution_run_immutable()`,
		`CREATE TABLE IF NOT EXISTS core_execution_run_stages (owner_id TEXT NOT NULL, run_id UUID NOT NULL, stage_id UUID NOT NULL, project_id UUID NOT NULL, plan_id UUID NOT NULL, plan_revision BIGINT NOT NULL CHECK(plan_revision>0), plan_digest TEXT NOT NULL CHECK(plan_digest ~ '^[a-f0-9]{64}$'), run_revision BIGINT NOT NULL CHECK(run_revision>0), plan_stage_key TEXT NOT NULL, stage_revision BIGINT NOT NULL CHECK(stage_revision>0), plan_stage_digest TEXT NOT NULL CHECK(plan_stage_digest ~ '^[a-f0-9]{64}$'), target_id UUID, target_revision BIGINT, target_digest TEXT CHECK(target_digest IS NULL OR target_digest ~ '^[a-f0-9]{64}$'), task_id UUID, confirmation_id UUID, ordinal BIGINT NOT NULL CHECK(ordinal>=0), revision BIGINT NOT NULL DEFAULT 1 CHECK(revision>0), status TEXT NOT NULL DEFAULT 'blocked' CHECK(status IN ('blocked','waiting_user','queued','running','succeeded','failed','uncertain','skipped','canceled','rejected','expired')), schema_version TEXT NOT NULL DEFAULT 'execution-run-stage/v2' CHECK(schema_version='execution-run-stage/v2'), snapshot_json JSONB NOT NULL DEFAULT '{}'::jsonb, started_at TIMESTAMPTZ, completed_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(owner_id,run_id,stage_id), UNIQUE(owner_id,run_id,ordinal), UNIQUE(owner_id,run_id,plan_id,plan_revision,plan_stage_key,stage_id), UNIQUE(owner_id,run_id,stage_id,project_id,plan_id,plan_revision,plan_stage_key), UNIQUE(owner_id,task_id), UNIQUE(owner_id,confirmation_id), CHECK((target_id IS NULL AND target_revision IS NULL AND target_digest IS NULL) OR (target_id IS NOT NULL AND target_revision IS NOT NULL AND target_revision>0 AND target_digest IS NOT NULL)), FOREIGN KEY(owner_id,run_id,project_id,plan_id,plan_revision) REFERENCES core_execution_runs(owner_id,run_id,project_id,plan_id,plan_revision) ON DELETE RESTRICT, FOREIGN KEY(owner_id,run_id,run_revision) REFERENCES core_execution_run_revisions(owner_id,run_id,revision) ON DELETE RESTRICT, FOREIGN KEY(owner_id,project_id,plan_id,plan_revision,plan_digest) REFERENCES core_execution_plan_revisions(owner_id,project_id,plan_id,revision,plan_digest) ON DELETE RESTRICT, FOREIGN KEY(owner_id,plan_id,plan_revision,plan_stage_key,stage_revision,plan_stage_digest) REFERENCES core_execution_plan_stages(owner_id,plan_id,plan_revision,stage_key,stage_revision,stage_digest) ON DELETE RESTRICT, FOREIGN KEY(owner_id,target_id,target_revision,target_digest) REFERENCES core_execution_targets(owner_id,target_id,target_revision,target_digest) ON DELETE RESTRICT)`,
		`CREATE TABLE IF NOT EXISTS core_execution_run_stage_dependencies (owner_id TEXT NOT NULL,run_id UUID NOT NULL,stage_id UUID NOT NULL,depends_on_stage_id UUID NOT NULL,PRIMARY KEY(owner_id,run_id,stage_id,depends_on_stage_id),CHECK(stage_id<>depends_on_stage_id),FOREIGN KEY(owner_id,run_id,stage_id) REFERENCES core_execution_run_stages(owner_id,run_id,stage_id) ON DELETE RESTRICT,FOREIGN KEY(owner_id,run_id,depends_on_stage_id) REFERENCES core_execution_run_stages(owner_id,run_id,stage_id) ON DELETE RESTRICT)`,
		`CREATE INDEX IF NOT EXISTS core_execution_run_stages_status_idx ON core_execution_run_stages(owner_id,run_id,status,ordinal)`,
		`CREATE OR REPLACE FUNCTION core_execution_run_stage_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF TG_OP='DELETE' OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.stage_id IS DISTINCT FROM OLD.stage_id OR NEW.project_id IS DISTINCT FROM OLD.project_id OR NEW.plan_id IS DISTINCT FROM OLD.plan_id OR NEW.plan_revision IS DISTINCT FROM OLD.plan_revision OR NEW.plan_digest IS DISTINCT FROM OLD.plan_digest OR NEW.run_revision IS DISTINCT FROM OLD.run_revision OR NEW.plan_stage_key IS DISTINCT FROM OLD.plan_stage_key OR NEW.stage_revision IS DISTINCT FROM OLD.stage_revision OR NEW.plan_stage_digest IS DISTINCT FROM OLD.plan_stage_digest OR NEW.created_at IS DISTINCT FROM OLD.created_at OR (OLD.started_at IS NOT NULL AND NEW.started_at IS DISTINCT FROM OLD.started_at) OR (OLD.completed_at IS NOT NULL AND NEW.completed_at IS DISTINCT FROM OLD.completed_at) OR (OLD.status IN ('running','succeeded','failed','uncertain','skipped','canceled','rejected','expired') AND NEW.status IN ('blocked','waiting_user','queued')) OR (OLD.status IN ('succeeded','failed','uncertain','skipped','canceled','rejected','expired') AND NEW.status IS DISTINCT FROM OLD.status) THEN RAISE EXCEPTION 'execution run stage identity/state is immutable'; END IF; RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_run_stages_immutable ON core_execution_run_stages`, `CREATE TRIGGER core_execution_run_stages_immutable BEFORE UPDATE OR DELETE ON core_execution_run_stages FOR EACH ROW EXECUTE FUNCTION core_execution_run_stage_immutable()`,
		`CREATE TABLE IF NOT EXISTS core_execution_step_attempts (owner_id TEXT NOT NULL, attempt_id UUID NOT NULL, run_id UUID NOT NULL, stage_id UUID NOT NULL, project_id UUID NOT NULL, plan_id UUID NOT NULL, plan_revision BIGINT NOT NULL CHECK(plan_revision>0), plan_stage_key TEXT NOT NULL, step_key TEXT NOT NULL, step_set TEXT NOT NULL DEFAULT 'forward' CHECK(step_set IN ('forward','rollback')), step_revision BIGINT NOT NULL CHECK(step_revision>0), step_digest TEXT NOT NULL CHECK(step_digest ~ '^[a-f0-9]{64}$'), attempt_no BIGINT NOT NULL CHECK(attempt_no>0), revision BIGINT NOT NULL DEFAULT 1 CHECK(revision>0), status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','queued','running','succeeded','failed','uncertain','canceled')), schema_version TEXT NOT NULL DEFAULT 'execution-step-attempt/v2' CHECK(schema_version='execution-step-attempt/v2'), input_digest TEXT CHECK(input_digest IS NULL OR input_digest ~ '^[a-f0-9]{64}$'), output_digest TEXT CHECK(output_digest IS NULL OR output_digest ~ '^[a-f0-9]{64}$'), snapshot_json JSONB NOT NULL DEFAULT '{}'::jsonb, started_at TIMESTAMPTZ, completed_at TIMESTAMPTZ, PRIMARY KEY(owner_id,attempt_id), UNIQUE(owner_id,run_id,attempt_id), UNIQUE(owner_id,run_id,stage_id,step_key,attempt_no), FOREIGN KEY(owner_id,run_id,stage_id,project_id,plan_id,plan_revision,plan_stage_key) REFERENCES core_execution_run_stages(owner_id,run_id,stage_id,project_id,plan_id,plan_revision,plan_stage_key) ON DELETE RESTRICT, FOREIGN KEY(owner_id,plan_id,plan_revision,plan_stage_key,step_set,step_key,step_revision,step_digest) REFERENCES core_execution_plan_steps(owner_id,plan_id,plan_revision,stage_key,step_set,step_key,step_revision,step_digest) ON DELETE RESTRICT)`,
		`CREATE INDEX IF NOT EXISTS core_execution_step_attempts_run_idx ON core_execution_step_attempts(owner_id,run_id,stage_id,step_key,attempt_no DESC)`,
		`CREATE OR REPLACE FUNCTION core_execution_step_attempt_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF TG_OP='DELETE' OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.attempt_id IS DISTINCT FROM OLD.attempt_id OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.stage_id IS DISTINCT FROM OLD.stage_id OR NEW.project_id IS DISTINCT FROM OLD.project_id OR NEW.plan_id IS DISTINCT FROM OLD.plan_id OR NEW.plan_revision IS DISTINCT FROM OLD.plan_revision OR NEW.plan_stage_key IS DISTINCT FROM OLD.plan_stage_key OR NEW.step_key IS DISTINCT FROM OLD.step_key OR NEW.step_revision IS DISTINCT FROM OLD.step_revision OR NEW.step_digest IS DISTINCT FROM OLD.step_digest OR (OLD.status IN ('running','succeeded','failed','uncertain','canceled') AND NEW.status IN ('pending','queued')) OR (OLD.status IN ('succeeded','failed','uncertain','canceled') AND NEW.status IS DISTINCT FROM OLD.status) THEN RAISE EXCEPTION 'execution step attempt identity/state is immutable'; END IF; RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_step_attempts_immutable ON core_execution_step_attempts`, `CREATE TRIGGER core_execution_step_attempts_immutable BEFORE UPDATE OR DELETE ON core_execution_step_attempts FOR EACH ROW EXECUTE FUNCTION core_execution_step_attempt_immutable()`,
		`CREATE TABLE IF NOT EXISTS core_execution_receipts (owner_id TEXT NOT NULL, receipt_id UUID NOT NULL, run_id UUID NOT NULL, attempt_id UUID, provider_operation_id TEXT NOT NULL DEFAULT '', command_id TEXT NOT NULL DEFAULT '', idempotency_digest TEXT NOT NULL CHECK(idempotency_digest ~ '^[a-f0-9]{64}$'), request_digest TEXT NOT NULL CHECK(request_digest ~ '^[a-f0-9]{64}$'), fence_digest TEXT NOT NULL CHECK(fence_digest ~ '^[a-f0-9]{64}$'), response_digest TEXT CHECK(response_digest IS NULL OR response_digest ~ '^[a-f0-9]{64}$'), revision BIGINT NOT NULL DEFAULT 1 CHECK(revision>0), status TEXT NOT NULL DEFAULT 'accepted' CHECK(status IN ('accepted','running','succeeded','failed','uncertain','canceled')), schema_version TEXT NOT NULL DEFAULT 'execution-receipt/v2' CHECK(schema_version='execution-receipt/v2'), snapshot_json JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(owner_id,receipt_id), UNIQUE(owner_id,run_id,receipt_id), UNIQUE(owner_id,run_id,idempotency_digest), UNIQUE(owner_id,fence_digest), FOREIGN KEY(owner_id,run_id) REFERENCES core_execution_runs(owner_id,run_id) ON DELETE RESTRICT, FOREIGN KEY(owner_id,run_id,attempt_id) REFERENCES core_execution_step_attempts(owner_id,run_id,attempt_id) ON DELETE RESTRICT)`,
		`CREATE INDEX IF NOT EXISTS core_execution_receipts_run_idx ON core_execution_receipts(owner_id,run_id,created_at,receipt_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS core_execution_receipts_provider_uidx ON core_execution_receipts(owner_id,provider_operation_id) WHERE provider_operation_id<>''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS core_execution_receipts_command_uidx ON core_execution_receipts(owner_id,command_id) WHERE command_id<>''`,
		`CREATE OR REPLACE FUNCTION core_execution_receipt_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF TG_OP='DELETE' OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.receipt_id IS DISTINCT FROM OLD.receipt_id OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.attempt_id IS DISTINCT FROM OLD.attempt_id OR NEW.schema_version IS DISTINCT FROM OLD.schema_version OR NEW.created_at IS DISTINCT FROM OLD.created_at OR NEW.request_digest IS DISTINCT FROM OLD.request_digest OR NEW.fence_digest IS DISTINCT FROM OLD.fence_digest OR (OLD.provider_operation_id<>'' AND NEW.provider_operation_id IS DISTINCT FROM OLD.provider_operation_id) OR (OLD.command_id<>'' AND NEW.command_id IS DISTINCT FROM OLD.command_id) OR NEW.idempotency_digest IS DISTINCT FROM OLD.idempotency_digest OR (OLD.status='accepted' AND NEW.status NOT IN ('accepted','running','succeeded','failed','uncertain','canceled')) OR (OLD.status='running' AND NEW.status NOT IN ('running','succeeded','failed','uncertain','canceled')) OR (NEW.status IN ('succeeded','failed','canceled') AND (NEW.response_digest IS NULL OR (NEW.provider_operation_id='' AND NEW.command_id=''))) OR (NEW.status='uncertain' AND NEW.response_digest IS NULL) OR (OLD.status IN ('succeeded','failed','uncertain','canceled') AND (NEW.status IS DISTINCT FROM OLD.status OR NEW.provider_operation_id IS DISTINCT FROM OLD.provider_operation_id OR NEW.command_id IS DISTINCT FROM OLD.command_id OR NEW.response_digest IS DISTINCT FROM OLD.response_digest OR NEW.snapshot_json IS DISTINCT FROM OLD.snapshot_json OR NEW.revision IS DISTINCT FROM OLD.revision)) THEN RAISE EXCEPTION 'execution receipt pin is immutable or regressive'; END IF; RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_receipts_immutable ON core_execution_receipts`, `CREATE TRIGGER core_execution_receipts_immutable BEFORE UPDATE OR DELETE ON core_execution_receipts FOR EACH ROW EXECUTE FUNCTION core_execution_receipt_immutable()`,
		`CREATE TABLE IF NOT EXISTS core_execution_artifacts (owner_id TEXT NOT NULL, artifact_id UUID NOT NULL, project_id UUID, plan_id UUID, plan_revision BIGINT, run_id UUID, attempt_id UUID, content_digest TEXT NOT NULL CHECK(content_digest ~ '^[a-f0-9]{64}$'), storage_backend TEXT NOT NULL DEFAULT 'filesystem' CHECK(storage_backend='filesystem'), storage_ref TEXT NOT NULL CHECK(storage_ref ~ '^sha256/[a-f0-9]{2}/[a-f0-9]{64}$'), size_bytes BIGINT NOT NULL DEFAULT 0 CHECK(size_bytes>=0), media_type TEXT NOT NULL DEFAULT 'application/octet-stream', revision BIGINT NOT NULL DEFAULT 1 CHECK(revision>0), status TEXT NOT NULL DEFAULT 'available' CHECK(status IN ('pending','available','unavailable')), schema_version TEXT NOT NULL DEFAULT 'execution-artifact/v2' CHECK(schema_version='execution-artifact/v2'), metadata_json JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(owner_id,artifact_id), CHECK(project_id IS NOT NULL AND plan_id IS NOT NULL AND plan_revision IS NOT NULL), CHECK((attempt_id IS NULL OR run_id IS NOT NULL) AND (run_id IS NULL OR attempt_id IS NULL OR (run_id IS NOT NULL AND attempt_id IS NOT NULL))), CHECK(right(storage_ref,64)=content_digest), FOREIGN KEY(owner_id,project_id) REFERENCES core_execution_projects(owner_id,project_id) ON DELETE RESTRICT, FOREIGN KEY(owner_id,plan_id,plan_revision) REFERENCES core_execution_plan_revisions(owner_id,plan_id,revision) ON DELETE RESTRICT, FOREIGN KEY(owner_id,run_id) REFERENCES core_execution_runs(owner_id,run_id) ON DELETE RESTRICT, FOREIGN KEY(owner_id,run_id,attempt_id) REFERENCES core_execution_step_attempts(owner_id,run_id,attempt_id) ON DELETE RESTRICT)`,
		`CREATE INDEX IF NOT EXISTS core_execution_artifacts_scope_idx ON core_execution_artifacts(owner_id,project_id,plan_id,plan_revision,run_id,created_at)`,
		`DROP INDEX IF EXISTS core_execution_artifacts_storage_uidx`, `CREATE INDEX IF NOT EXISTS core_execution_artifacts_storage_idx ON core_execution_artifacts(owner_id,storage_backend,storage_ref)`,
		`CREATE OR REPLACE FUNCTION core_execution_artifact_scope_guard() RETURNS trigger LANGUAGE plpgsql AS $$ DECLARE run_project UUID; plan_project UUID; BEGIN IF TG_OP='DELETE' THEN RAISE EXCEPTION 'execution artifacts are immutable'; END IF; IF TG_OP='UPDATE' AND (NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.artifact_id IS DISTINCT FROM OLD.artifact_id OR NEW.project_id IS DISTINCT FROM OLD.project_id OR NEW.plan_id IS DISTINCT FROM OLD.plan_id OR NEW.plan_revision IS DISTINCT FROM OLD.plan_revision OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.attempt_id IS DISTINCT FROM OLD.attempt_id OR NEW.content_digest IS DISTINCT FROM OLD.content_digest OR NEW.storage_backend IS DISTINCT FROM OLD.storage_backend OR NEW.storage_ref IS DISTINCT FROM OLD.storage_ref OR NEW.size_bytes IS DISTINCT FROM OLD.size_bytes OR NEW.media_type IS DISTINCT FROM OLD.media_type OR NEW.revision IS DISTINCT FROM OLD.revision OR NEW.status IS DISTINCT FROM OLD.status OR NEW.schema_version IS DISTINCT FROM OLD.schema_version OR NEW.metadata_json IS DISTINCT FROM OLD.metadata_json OR NEW.created_at IS DISTINCT FROM OLD.created_at) THEN RAISE EXCEPTION 'artifact identity or evidence is immutable'; END IF; IF NEW.run_id IS NOT NULL THEN SELECT project_id INTO run_project FROM core_execution_runs WHERE owner_id=NEW.owner_id AND run_id=NEW.run_id; IF NEW.project_id IS DISTINCT FROM run_project THEN RAISE EXCEPTION 'artifact project/run scope mismatch'; END IF; END IF; IF NEW.plan_id IS NOT NULL THEN SELECT project_id INTO plan_project FROM core_execution_plans WHERE owner_id=NEW.owner_id AND plan_id=NEW.plan_id; IF NEW.project_id IS DISTINCT FROM plan_project THEN RAISE EXCEPTION 'artifact project/plan scope mismatch'; END IF; END IF; RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_artifacts_scope_guard ON core_execution_artifacts`, `CREATE TRIGGER core_execution_artifacts_scope_guard BEFORE INSERT OR UPDATE OR DELETE ON core_execution_artifacts FOR EACH ROW EXECUTE FUNCTION core_execution_artifact_scope_guard()`,
		`CREATE TABLE IF NOT EXISTS core_execution_service_bindings (owner_id TEXT NOT NULL, binding_id UUID NOT NULL, deployment_id UUID NOT NULL, release_id TEXT NOT NULL DEFAULT '', project_id UUID NOT NULL, run_id UUID NOT NULL, target_id UUID, target_revision BIGINT, protocol TEXT NOT NULL, endpoint TEXT NOT NULL, binding_digest TEXT NOT NULL CHECK(binding_digest ~ '^[a-f0-9]{64}$'), operation_schema_digest TEXT NOT NULL CHECK(operation_schema_digest ~ '^[a-f0-9]{64}$'), usage_artifact_id UUID, health_artifact_id UUID, revision BIGINT NOT NULL DEFAULT 1 CHECK(revision>0), status TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active','disabled','revoked')), schema_version TEXT NOT NULL DEFAULT 'execution-service-binding/v2' CHECK(schema_version='execution-service-binding/v2'), snapshot_json JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(owner_id,binding_id), UNIQUE(owner_id,deployment_id,protocol,endpoint), CHECK((target_id IS NULL AND target_revision IS NULL) OR (target_id IS NOT NULL AND target_revision IS NOT NULL AND target_revision>0)), FOREIGN KEY(owner_id,deployment_id) REFERENCES core_execution_deployments(owner_id,deployment_id) ON DELETE RESTRICT, FOREIGN KEY(owner_id,project_id) REFERENCES core_execution_projects(owner_id,project_id) ON DELETE RESTRICT, FOREIGN KEY(owner_id,run_id) REFERENCES core_execution_runs(owner_id,run_id) ON DELETE RESTRICT, FOREIGN KEY(owner_id,target_id,target_revision) REFERENCES core_execution_targets(owner_id,target_id,target_revision) ON DELETE RESTRICT, FOREIGN KEY(owner_id,usage_artifact_id) REFERENCES core_execution_artifacts(owner_id,artifact_id) ON DELETE RESTRICT, FOREIGN KEY(owner_id,health_artifact_id) REFERENCES core_execution_artifacts(owner_id,artifact_id) ON DELETE RESTRICT)`,
		`CREATE TABLE IF NOT EXISTS core_execution_event_counters (
				owner_id TEXT NOT NULL, run_id UUID NOT NULL, next_sequence BIGINT NOT NULL DEFAULT 1 CHECK(next_sequence>0), PRIMARY KEY(owner_id,run_id), FOREIGN KEY(owner_id,run_id) REFERENCES core_execution_runs(owner_id,run_id) ON DELETE CASCADE
			)`,
		`CREATE TABLE IF NOT EXISTS core_execution_events (owner_id TEXT NOT NULL, run_id UUID NOT NULL, event_id UUID NOT NULL, sequence BIGINT NOT NULL CHECK(sequence>0), stage_id UUID, attempt_id UUID, step_key TEXT, kind TEXT NOT NULL CHECK(kind ~ '^[a-z][a-z0-9_.-]*$'), event_key TEXT NOT NULL DEFAULT '', event_digest TEXT CHECK(event_digest IS NULL OR event_digest ~ '^[a-f0-9]{64}$'), status TEXT NOT NULL DEFAULT 'recorded' CHECK(status IN ('recorded','pending','running','succeeded','failed','uncertain','canceled')), revision BIGINT NOT NULL DEFAULT 1 CHECK(revision>0), schema_version TEXT NOT NULL DEFAULT 'execution-event/v2' CHECK(schema_version='execution-event/v2'), event_json JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(owner_id,run_id,sequence), UNIQUE(owner_id,event_id), CHECK(event_digest IS NOT NULL OR event_json ? 'digest'), FOREIGN KEY(owner_id,run_id) REFERENCES core_execution_runs(owner_id,run_id) ON DELETE RESTRICT, FOREIGN KEY(owner_id,run_id,stage_id) REFERENCES core_execution_run_stages(owner_id,run_id,stage_id) ON DELETE RESTRICT, FOREIGN KEY(owner_id,run_id,attempt_id) REFERENCES core_execution_step_attempts(owner_id,run_id,attempt_id) ON DELETE RESTRICT)`,
		`CREATE INDEX IF NOT EXISTS core_execution_events_run_idx ON core_execution_events(owner_id,run_id,created_at,sequence)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS core_execution_events_key_uidx ON core_execution_events(owner_id,run_id,event_key) WHERE event_key<>''`,
		`CREATE OR REPLACE FUNCTION core_execution_event_scope_guard() RETURNS trigger LANGUAGE plpgsql AS $$ DECLARE attempt_stage UUID; attempt_step TEXT; BEGIN IF NEW.attempt_id IS NOT NULL THEN SELECT stage_id,step_key INTO attempt_stage,attempt_step FROM core_execution_step_attempts WHERE owner_id=NEW.owner_id AND run_id=NEW.run_id AND attempt_id=NEW.attempt_id; IF NEW.stage_id IS DISTINCT FROM attempt_stage OR NEW.step_key IS DISTINCT FROM attempt_step THEN RAISE EXCEPTION 'event stage/attempt scope mismatch'; END IF; END IF; RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_events_scope_guard ON core_execution_events`, `CREATE TRIGGER core_execution_events_scope_guard BEFORE INSERT OR UPDATE ON core_execution_events FOR EACH ROW EXECUTE FUNCTION core_execution_event_scope_guard()`,
		`CREATE OR REPLACE FUNCTION core_execution_event_append_only() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'execution events are append-only'; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_events_append_only ON core_execution_events`, `CREATE TRIGGER core_execution_events_append_only BEFORE UPDATE OR DELETE ON core_execution_events FOR EACH ROW EXECUTE FUNCTION core_execution_event_append_only()`,
		`CREATE TABLE IF NOT EXISTS core_execution_idempotency (owner_id TEXT NOT NULL, idempotency_id UUID NOT NULL, run_id UUID, key_digest TEXT NOT NULL CHECK(key_digest ~ '^[a-f0-9]{64}$'), request_digest TEXT NOT NULL CHECK(request_digest ~ '^[a-f0-9]{64}$'), response_digest TEXT CHECK(response_digest IS NULL OR response_digest ~ '^[a-f0-9]{64}$'), revision BIGINT NOT NULL DEFAULT 1 CHECK(revision>0), status TEXT NOT NULL DEFAULT 'accepted' CHECK(status IN ('accepted','running','succeeded','failed','uncertain','canceled')), schema_version TEXT NOT NULL DEFAULT 'execution-idempotency/v2' CHECK(schema_version='execution-idempotency/v2'), response_json JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(owner_id,idempotency_id), UNIQUE(owner_id,key_digest), CHECK(status IN ('accepted','running') OR response_digest IS NOT NULL), FOREIGN KEY(owner_id,run_id) REFERENCES core_execution_runs(owner_id,run_id) ON DELETE RESTRICT)`,
		`CREATE INDEX IF NOT EXISTS core_execution_idempotency_run_idx ON core_execution_idempotency(owner_id,run_id,created_at) WHERE run_id IS NOT NULL`,
		`CREATE OR REPLACE FUNCTION core_execution_idempotency_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF TG_OP='DELETE' OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.idempotency_id IS DISTINCT FROM OLD.idempotency_id OR NEW.schema_version IS DISTINCT FROM OLD.schema_version OR NEW.created_at IS DISTINCT FROM OLD.created_at OR NEW.key_digest IS DISTINCT FROM OLD.key_digest OR NEW.request_digest IS DISTINCT FROM OLD.request_digest OR NEW.run_id IS DISTINCT FROM OLD.run_id OR (OLD.response_digest IS NOT NULL AND (NEW.response_digest IS DISTINCT FROM OLD.response_digest OR NEW.response_json IS DISTINCT FROM OLD.response_json)) OR (OLD.status='accepted' AND NEW.status NOT IN ('accepted','running','succeeded','failed','uncertain','canceled')) OR (OLD.status='running' AND NEW.status NOT IN ('running','succeeded','failed','uncertain','canceled')) OR (OLD.status IN ('succeeded','failed','uncertain','canceled') AND NEW.status IS DISTINCT FROM OLD.status) OR (NEW.status IN ('succeeded','failed','uncertain','canceled') AND NEW.response_digest IS NULL) THEN RAISE EXCEPTION 'execution idempotency evidence is immutable or regressive'; END IF; RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_idempotency_immutable ON core_execution_idempotency`, `CREATE TRIGGER core_execution_idempotency_immutable BEFORE UPDATE OR DELETE ON core_execution_idempotency FOR EACH ROW EXECUTE FUNCTION core_execution_idempotency_immutable()`,
		`CREATE TABLE IF NOT EXISTS core_execution_target_mutation_leases (owner_id TEXT NOT NULL, target_id UUID NOT NULL, target_revision BIGINT NOT NULL CHECK(target_revision>0), lease_id UUID NOT NULL, run_id UUID, stage_id UUID, provider_operation_id TEXT NOT NULL DEFAULT '', receipt_id UUID, token UUID, epoch BIGINT NOT NULL DEFAULT 0 CHECK(epoch>=0), expires_at TIMESTAMPTZ, revision BIGINT NOT NULL DEFAULT 1 CHECK(revision>0), status TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active','uncertain','released')), schema_version TEXT NOT NULL DEFAULT 'execution-target-lease/v2' CHECK(schema_version='execution-target-lease/v2'), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(owner_id,target_id,target_revision), UNIQUE(owner_id,lease_id), CHECK((status='uncertain' AND run_id IS NOT NULL AND receipt_id IS NOT NULL) OR status<>'uncertain'), CHECK((status<>'active') OR (run_id IS NOT NULL AND token IS NOT NULL AND expires_at IS NOT NULL)), FOREIGN KEY(owner_id,target_id,target_revision) REFERENCES core_execution_targets(owner_id,target_id,target_revision) ON DELETE RESTRICT, FOREIGN KEY(owner_id,run_id) REFERENCES core_execution_runs(owner_id,run_id) ON DELETE RESTRICT, FOREIGN KEY(owner_id,run_id,stage_id) REFERENCES core_execution_run_stages(owner_id,run_id,stage_id) ON DELETE RESTRICT, FOREIGN KEY(owner_id,run_id,receipt_id) REFERENCES core_execution_receipts(owner_id,run_id,receipt_id) ON DELETE RESTRICT)`,
		`CREATE INDEX IF NOT EXISTS core_execution_target_mutation_leases_expiry_idx ON core_execution_target_mutation_leases(owner_id,target_id,target_revision,expires_at)`,
		`CREATE TABLE IF NOT EXISTS core_execution_dispatch_intents (owner_id TEXT NOT NULL, intent_id UUID NOT NULL, run_id UUID NOT NULL, stage_id UUID NOT NULL, attempt_id UUID NOT NULL, receipt_id UUID NOT NULL, task_id UUID NOT NULL, task_lease_epoch BIGINT NOT NULL CHECK(task_lease_epoch>0), target_id UUID NOT NULL, target_revision BIGINT NOT NULL CHECK(target_revision>0), target_digest TEXT NOT NULL CHECK(target_digest ~ '^[a-f0-9]{64}$'), plan_id UUID NOT NULL, plan_revision BIGINT NOT NULL CHECK(plan_revision>0), plan_digest TEXT NOT NULL CHECK(plan_digest ~ '^[a-f0-9]{64}$'), stage_revision BIGINT NOT NULL CHECK(stage_revision>0), stage_digest TEXT NOT NULL CHECK(stage_digest ~ '^[a-f0-9]{64}$'), step_key TEXT NOT NULL, step_set TEXT NOT NULL CHECK(step_set IN ('forward','rollback')), step_revision BIGINT NOT NULL CHECK(step_revision>0), step_digest TEXT NOT NULL CHECK(step_digest ~ '^[a-f0-9]{64}$'), attempt_no BIGINT NOT NULL CHECK(attempt_no>0), lease_id UUID NOT NULL, lease_token UUID NOT NULL, lease_epoch BIGINT NOT NULL CHECK(lease_epoch>0), request_digest TEXT NOT NULL CHECK(request_digest ~ '^[a-f0-9]{64}$'), fence_digest TEXT NOT NULL CHECK(fence_digest ~ '^[a-f0-9]{64}$'), status TEXT NOT NULL DEFAULT 'intent' CHECK(status IN ('intent','uncertain','accepted','succeeded','failed','canceled')), revision BIGINT NOT NULL DEFAULT 1 CHECK(revision>0), schema_version TEXT NOT NULL DEFAULT 'execution-dispatch-intent/v2' CHECK(schema_version='execution-dispatch-intent/v2'), snapshot_json JSONB NOT NULL CHECK(jsonb_typeof(snapshot_json)='object' AND snapshot_json<>'{}'::jsonb), created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(owner_id,intent_id), UNIQUE(owner_id,fence_digest), UNIQUE(owner_id,run_id,stage_id,step_key,step_set,attempt_no), FOREIGN KEY(owner_id,run_id,stage_id) REFERENCES core_execution_run_stages(owner_id,run_id,stage_id) ON DELETE RESTRICT, FOREIGN KEY(owner_id,run_id,attempt_id) REFERENCES core_execution_step_attempts(owner_id,run_id,attempt_id) ON DELETE RESTRICT, FOREIGN KEY(owner_id,run_id,receipt_id) REFERENCES core_execution_receipts(owner_id,run_id,receipt_id) ON DELETE RESTRICT, FOREIGN KEY(owner_id,task_id) REFERENCES agent_tasks(owner_id,task_id) ON DELETE RESTRICT, FOREIGN KEY(owner_id,target_id,target_revision) REFERENCES core_execution_targets(owner_id,target_id,target_revision) ON DELETE RESTRICT, FOREIGN KEY(owner_id,lease_id) REFERENCES core_execution_target_mutation_leases(owner_id,lease_id) ON DELETE RESTRICT)`,
		`CREATE INDEX IF NOT EXISTS core_execution_dispatch_intents_run_idx ON core_execution_dispatch_intents(owner_id,run_id,stage_id,status,created_at)`,
		`CREATE OR REPLACE FUNCTION core_execution_dispatch_intent_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF TG_OP='DELETE' OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.intent_id IS DISTINCT FROM OLD.intent_id OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.stage_id IS DISTINCT FROM OLD.stage_id OR NEW.attempt_id IS DISTINCT FROM OLD.attempt_id OR NEW.receipt_id IS DISTINCT FROM OLD.receipt_id OR NEW.task_id IS DISTINCT FROM OLD.task_id OR NEW.task_lease_epoch IS DISTINCT FROM OLD.task_lease_epoch OR NEW.target_id IS DISTINCT FROM OLD.target_id OR NEW.target_revision IS DISTINCT FROM OLD.target_revision OR NEW.target_digest IS DISTINCT FROM OLD.target_digest OR NEW.plan_id IS DISTINCT FROM OLD.plan_id OR NEW.plan_revision IS DISTINCT FROM OLD.plan_revision OR NEW.plan_digest IS DISTINCT FROM OLD.plan_digest OR NEW.stage_revision IS DISTINCT FROM OLD.stage_revision OR NEW.stage_digest IS DISTINCT FROM OLD.stage_digest OR NEW.step_key IS DISTINCT FROM OLD.step_key OR NEW.step_set IS DISTINCT FROM OLD.step_set OR NEW.step_revision IS DISTINCT FROM OLD.step_revision OR NEW.step_digest IS DISTINCT FROM OLD.step_digest OR NEW.attempt_no IS DISTINCT FROM OLD.attempt_no OR NEW.lease_id IS DISTINCT FROM OLD.lease_id OR NEW.lease_token IS DISTINCT FROM OLD.lease_token OR NEW.lease_epoch IS DISTINCT FROM OLD.lease_epoch OR NEW.request_digest IS DISTINCT FROM OLD.request_digest OR NEW.fence_digest IS DISTINCT FROM OLD.fence_digest OR NEW.schema_version IS DISTINCT FROM OLD.schema_version OR NEW.snapshot_json IS DISTINCT FROM OLD.snapshot_json OR NEW.created_at IS DISTINCT FROM OLD.created_at OR (OLD.status='intent' AND NEW.status NOT IN ('intent','accepted','uncertain','succeeded','failed','canceled')) OR (OLD.status='accepted' AND NEW.status NOT IN ('accepted','uncertain','succeeded','failed','canceled')) OR (OLD.status='uncertain' AND NEW.status IS DISTINCT FROM 'uncertain') OR (OLD.status IN ('succeeded','failed','canceled') AND NEW.status IS DISTINCT FROM OLD.status) THEN RAISE EXCEPTION 'execution dispatch intent identity/state is immutable or regressive'; END IF; RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_dispatch_intents_immutable ON core_execution_dispatch_intents`, `CREATE TRIGGER core_execution_dispatch_intents_immutable BEFORE UPDATE OR DELETE ON core_execution_dispatch_intents FOR EACH ROW EXECUTE FUNCTION core_execution_dispatch_intent_immutable()`,
		`CREATE TABLE IF NOT EXISTS core_execution_secret_parameter_intents (owner_id TEXT NOT NULL CHECK(owner_id<>''),fence_digest TEXT NOT NULL CHECK(fence_digest ~ '^[a-f0-9]{64}$'),request_digest TEXT NOT NULL CHECK(request_digest ~ '^[a-f0-9]{64}$'),parameter_name TEXT NOT NULL CHECK(parameter_name ~ '^/dirextalk/execution-v2/[0-9a-f-]{36}/[0-9a-f-]{36}/[a-f0-9]{32}$'),run_id UUID NOT NULL,provision_stage_id UUID NOT NULL,provision_attempt_id UUID NOT NULL,target_id UUID NOT NULL,target_revision BIGINT NOT NULL CHECK(target_revision>0),target_digest TEXT NOT NULL CHECK(target_digest ~ '^[a-f0-9]{64}$'),secret_ref UUID NOT NULL,secret_revision BIGINT NOT NULL CHECK(secret_revision>0),secret_purpose TEXT NOT NULL CHECK(secret_purpose='ai_provider_api_key'),secret_binding_digest TEXT NOT NULL CHECK(secret_binding_digest ~ '^[a-f0-9]{64}$'),confirmation_id UUID NOT NULL,status TEXT NOT NULL DEFAULT 'reserved' CHECK(status IN ('reserved','versioned','uncertain','completed','revoked')),provider_version BIGINT NOT NULL DEFAULT 0 CHECK(provider_version>=0),revision BIGINT NOT NULL DEFAULT 1 CHECK(revision>0),schema_version TEXT NOT NULL DEFAULT 'execution-secret-parameter-intent/v1' CHECK(schema_version='execution-secret-parameter-intent/v1'),request_json JSONB NOT NULL CHECK(jsonb_typeof(request_json)='object' AND request_json<>'{}'::jsonb),lease_json JSONB,lease_digest TEXT CHECK(lease_digest IS NULL OR lease_digest ~ '^[a-f0-9]{64}$'),created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),PRIMARY KEY(owner_id,fence_digest),UNIQUE(owner_id,parameter_name),CHECK((lease_json IS NULL)=(lease_digest IS NULL)),CHECK(status NOT IN ('versioned','completed') OR provider_version>0),CHECK(status<>'completed' OR lease_json IS NOT NULL),FOREIGN KEY(owner_id,fence_digest) REFERENCES core_execution_dispatch_intents(owner_id,fence_digest) ON DELETE RESTRICT,FOREIGN KEY(owner_id,run_id,provision_stage_id) REFERENCES core_execution_run_stages(owner_id,run_id,stage_id) ON DELETE RESTRICT,FOREIGN KEY(owner_id,run_id,provision_attempt_id) REFERENCES core_execution_step_attempts(owner_id,run_id,attempt_id) ON DELETE RESTRICT,FOREIGN KEY(owner_id,target_id,target_revision,target_digest) REFERENCES core_execution_targets(owner_id,target_id,target_revision,target_digest) ON DELETE RESTRICT,FOREIGN KEY(owner_id,secret_ref,secret_revision) REFERENCES core_execution_secrets(owner_id,secret_ref,revision) ON DELETE RESTRICT,FOREIGN KEY(owner_id,confirmation_id) REFERENCES agent_confirmations(owner_id,confirmation_id) ON DELETE RESTRICT)`,
		`CREATE INDEX IF NOT EXISTS core_execution_secret_parameter_intents_run_idx ON core_execution_secret_parameter_intents(owner_id,run_id,provision_stage_id,status,updated_at)`,
		`CREATE INDEX IF NOT EXISTS core_execution_secret_parameter_intents_secret_idx ON core_execution_secret_parameter_intents(owner_id,secret_ref,secret_revision,status)`,
		`CREATE OR REPLACE FUNCTION core_execution_secret_parameter_intent_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF TG_OP='DELETE' OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.fence_digest IS DISTINCT FROM OLD.fence_digest OR NEW.request_digest IS DISTINCT FROM OLD.request_digest OR NEW.parameter_name IS DISTINCT FROM OLD.parameter_name OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.provision_stage_id IS DISTINCT FROM OLD.provision_stage_id OR NEW.provision_attempt_id IS DISTINCT FROM OLD.provision_attempt_id OR NEW.target_id IS DISTINCT FROM OLD.target_id OR NEW.target_revision IS DISTINCT FROM OLD.target_revision OR NEW.target_digest IS DISTINCT FROM OLD.target_digest OR NEW.secret_ref IS DISTINCT FROM OLD.secret_ref OR NEW.secret_revision IS DISTINCT FROM OLD.secret_revision OR NEW.secret_purpose IS DISTINCT FROM OLD.secret_purpose OR NEW.secret_binding_digest IS DISTINCT FROM OLD.secret_binding_digest OR NEW.confirmation_id IS DISTINCT FROM OLD.confirmation_id OR NEW.schema_version IS DISTINCT FROM OLD.schema_version OR NEW.request_json IS DISTINCT FROM OLD.request_json OR NEW.created_at IS DISTINCT FROM OLD.created_at OR NEW.revision<>OLD.revision+1 OR (OLD.provider_version>0 AND NEW.provider_version IS DISTINCT FROM OLD.provider_version) OR (OLD.lease_json IS NOT NULL AND (NEW.lease_json IS DISTINCT FROM OLD.lease_json OR NEW.lease_digest IS DISTINCT FROM OLD.lease_digest)) OR (OLD.status='reserved' AND NEW.status NOT IN ('reserved','versioned','uncertain','revoked')) OR (OLD.status='versioned' AND NEW.status NOT IN ('versioned','uncertain','completed','revoked')) OR (OLD.status='uncertain' AND NEW.status NOT IN ('uncertain','completed','revoked')) OR (OLD.status='completed' AND NEW.status NOT IN ('completed','revoked')) OR (OLD.status='revoked' AND NEW.status<>'revoked') THEN RAISE EXCEPTION 'secret parameter intent identity/evidence is immutable or regressive'; END IF; RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_secret_parameter_intents_guard ON core_execution_secret_parameter_intents`, `CREATE TRIGGER core_execution_secret_parameter_intents_guard BEFORE UPDATE OR DELETE ON core_execution_secret_parameter_intents FOR EACH ROW EXECUTE FUNCTION core_execution_secret_parameter_intent_guard()`,
		`CREATE TABLE IF NOT EXISTS core_execution_ec2_provision_intents (owner_id TEXT NOT NULL CHECK(owner_id<>''),fence_digest TEXT NOT NULL CHECK(fence_digest ~ '^[a-f0-9]{64}$'),request_digest TEXT NOT NULL CHECK(request_digest ~ '^[a-f0-9]{64}$'),provider_operation_key TEXT NOT NULL CHECK(provider_operation_key<>''),provider_operation_id TEXT NOT NULL DEFAULT '',run_id UUID NOT NULL,stage_id UUID NOT NULL,attempt_id UUID NOT NULL,receipt_id UUID NOT NULL,target_id UUID NOT NULL,target_revision BIGINT NOT NULL CHECK(target_revision=1),target_digest TEXT NOT NULL CHECK(target_digest ~ '^[a-f0-9]{64}$'),plan_id UUID NOT NULL,plan_revision BIGINT NOT NULL CHECK(plan_revision>0),plan_digest TEXT NOT NULL CHECK(plan_digest ~ '^[a-f0-9]{64}$'),policy_digest TEXT NOT NULL CHECK(policy_digest ~ '^[a-f0-9]{64}$'),cost_quote_digest TEXT NOT NULL CHECK(cost_quote_digest ~ '^[a-f0-9]{64}$'),status TEXT NOT NULL DEFAULT 'intent' CHECK(status IN ('intent','accepted','pending','uncertain','succeeded','failed')),revision BIGINT NOT NULL DEFAULT 1 CHECK(revision>0),schema_version TEXT NOT NULL DEFAULT 'execution-ec2-provision-intent/v2' CHECK(schema_version='execution-ec2-provision-intent/v2'),request_json JSONB NOT NULL CHECK(jsonb_typeof(request_json)='object' AND request_json<>'{}'::jsonb),readback_digest TEXT CHECK(readback_digest IS NULL OR readback_digest ~ '^[a-f0-9]{64}$'),readback_json JSONB,CHECK((readback_digest IS NULL)=(readback_json IS NULL)),created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),PRIMARY KEY(owner_id,fence_digest),UNIQUE(owner_id,provider_operation_key),FOREIGN KEY(owner_id,fence_digest) REFERENCES core_execution_dispatch_intents(owner_id,fence_digest) ON DELETE RESTRICT,FOREIGN KEY(owner_id,run_id,stage_id) REFERENCES core_execution_run_stages(owner_id,run_id,stage_id) ON DELETE RESTRICT,FOREIGN KEY(owner_id,run_id,attempt_id) REFERENCES core_execution_step_attempts(owner_id,run_id,attempt_id) ON DELETE RESTRICT,FOREIGN KEY(owner_id,run_id,receipt_id) REFERENCES core_execution_receipts(owner_id,run_id,receipt_id) ON DELETE RESTRICT,FOREIGN KEY(owner_id,target_id,target_revision,target_digest) REFERENCES core_execution_targets(owner_id,target_id,target_revision,target_digest) ON DELETE RESTRICT,FOREIGN KEY(owner_id,plan_id,plan_revision) REFERENCES core_execution_plan_revisions(owner_id,plan_id,revision) ON DELETE RESTRICT)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS core_execution_ec2_provision_provider_uidx ON core_execution_ec2_provision_intents(owner_id,provider_operation_id) WHERE provider_operation_id<>''`,
		`CREATE INDEX IF NOT EXISTS core_execution_ec2_provision_intents_run_idx ON core_execution_ec2_provision_intents(owner_id,run_id,stage_id,status,updated_at)`,
		`CREATE OR REPLACE FUNCTION core_execution_ec2_provision_intent_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF TG_OP='DELETE' OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.fence_digest IS DISTINCT FROM OLD.fence_digest OR NEW.request_digest IS DISTINCT FROM OLD.request_digest OR NEW.provider_operation_key IS DISTINCT FROM OLD.provider_operation_key OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.stage_id IS DISTINCT FROM OLD.stage_id OR NEW.attempt_id IS DISTINCT FROM OLD.attempt_id OR NEW.receipt_id IS DISTINCT FROM OLD.receipt_id OR NEW.target_id IS DISTINCT FROM OLD.target_id OR NEW.target_revision IS DISTINCT FROM OLD.target_revision OR NEW.target_digest IS DISTINCT FROM OLD.target_digest OR NEW.plan_id IS DISTINCT FROM OLD.plan_id OR NEW.plan_revision IS DISTINCT FROM OLD.plan_revision OR NEW.plan_digest IS DISTINCT FROM OLD.plan_digest OR NEW.policy_digest IS DISTINCT FROM OLD.policy_digest OR NEW.cost_quote_digest IS DISTINCT FROM OLD.cost_quote_digest OR NEW.schema_version IS DISTINCT FROM OLD.schema_version OR NEW.request_json IS DISTINCT FROM OLD.request_json OR NEW.created_at IS DISTINCT FROM OLD.created_at OR NEW.revision<>OLD.revision+1 OR (OLD.provider_operation_id<>'' AND NEW.provider_operation_id IS DISTINCT FROM OLD.provider_operation_id) OR (OLD.status='intent' AND NEW.status NOT IN ('intent','accepted','pending','uncertain','succeeded','failed')) OR (OLD.status='accepted' AND NEW.status NOT IN ('accepted','pending','uncertain','succeeded','failed')) OR (OLD.status='pending' AND NEW.status NOT IN ('pending','uncertain','succeeded','failed')) OR (OLD.status='uncertain' AND NEW.status NOT IN ('uncertain','succeeded','failed')) OR (OLD.status IN ('succeeded','failed') AND (NEW.status IS DISTINCT FROM OLD.status OR NEW.readback_digest IS DISTINCT FROM OLD.readback_digest OR NEW.readback_json IS DISTINCT FROM OLD.readback_json)) THEN RAISE EXCEPTION 'ec2 provision intent identity/evidence is immutable or regressive'; END IF; RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_ec2_provision_intents_guard ON core_execution_ec2_provision_intents`, `CREATE TRIGGER core_execution_ec2_provision_intents_guard BEFORE UPDATE OR DELETE ON core_execution_ec2_provision_intents FOR EACH ROW EXECUTE FUNCTION core_execution_ec2_provision_intent_guard()`,
	}); err != nil {
		return err
	}
	if err := execMigrationStatements(ctx, txn, []string{
		`CREATE TABLE IF NOT EXISTS core_execution_runtime_concurrency (singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK(singleton),running_count BIGINT NOT NULL DEFAULT 0 CHECK(running_count>=0),max_concurrent BIGINT NOT NULL DEFAULT 1 CHECK(max_concurrent>0),revision BIGINT NOT NULL DEFAULT 1 CHECK(revision>0),updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE IF NOT EXISTS core_execution_reconciliation_resolutions (owner_id TEXT NOT NULL,run_id UUID NOT NULL,stage_id UUID NOT NULL,lease_id UUID NOT NULL,token UUID NOT NULL,epoch BIGINT NOT NULL CHECK(epoch>=0),receipt_id UUID NOT NULL,provider_operation_id TEXT NOT NULL,request_digest TEXT NOT NULL CHECK(request_digest ~ '^[a-f0-9]{64}$'),outcome TEXT NOT NULL CHECK(outcome IN ('succeeded','failed','uncertain','canceled')),outcome_digest TEXT NOT NULL CHECK(outcome_digest ~ '^[a-f0-9]{64}$'),observed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),PRIMARY KEY(owner_id,run_id,stage_id,lease_id,epoch),UNIQUE(owner_id,run_id,stage_id,request_digest),FOREIGN KEY(owner_id,run_id,stage_id) REFERENCES core_execution_run_stages(owner_id,run_id,stage_id) ON DELETE RESTRICT,FOREIGN KEY(owner_id,run_id,receipt_id) REFERENCES core_execution_receipts(owner_id,run_id,receipt_id) ON DELETE RESTRICT,FOREIGN KEY(owner_id,lease_id) REFERENCES core_execution_target_mutation_leases(owner_id,lease_id) ON DELETE RESTRICT)`,
		`CREATE OR REPLACE FUNCTION core_execution_run_stage_task_scope_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.task_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM agent_tasks WHERE owner_id=NEW.owner_id AND task_id=NEW.task_id AND spec_json->>'kind'='execution_stage') THEN RAISE EXCEPTION 'execution stage task must be owner-scoped execution_stage'; END IF; IF NEW.confirmation_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM agent_confirmations WHERE owner_id=NEW.owner_id AND confirmation_id=NEW.confirmation_id AND task_id=NEW.task_id) THEN RAISE EXCEPTION 'execution stage confirmation must bind its owner-scoped task'; END IF; RETURN NEW; END $$`,
		`CREATE OR REPLACE FUNCTION core_execution_run_stage_exact_scope_guard() RETURNS trigger LANGUAGE plpgsql AS $$ DECLARE p JSONB; b JSONB; BEGIN IF NEW.task_id IS NOT NULL THEN SELECT spec_json->'payload'->'execution_stage' INTO p FROM agent_tasks WHERE owner_id=NEW.owner_id AND task_id=NEW.task_id; IF p IS NULL OR p->>'plan_id'<>NEW.plan_id::text OR (p->>'plan_revision')::bigint<>NEW.plan_revision OR p->>'plan_digest' IS DISTINCT FROM NEW.plan_digest OR p->>'run_id'<>NEW.run_id::text OR COALESCE(p->>'run_revision','') IS DISTINCT FROM NEW.run_revision::text OR p->>'stage_id'<>NEW.stage_id::text OR (p->>'stage_revision')::bigint<>NEW.stage_revision OR p->>'stage_digest'<>NEW.plan_stage_digest OR p->>'target_id' IS DISTINCT FROM COALESCE(NEW.target_id::text,'') OR COALESCE(p->>'target_revision','') IS DISTINCT FROM COALESCE(NEW.target_revision::text,'') OR p->>'target_digest' IS DISTINCT FROM COALESCE(NEW.target_digest,'') THEN RAISE EXCEPTION 'execution stage task immutable scope mismatch'; END IF; END IF; IF NEW.confirmation_id IS NOT NULL THEN SELECT binding_json INTO b FROM agent_confirmations WHERE owner_id=NEW.owner_id AND confirmation_id=NEW.confirmation_id; IF b->>'plan_id'<>NEW.plan_id::text OR (b->>'plan_revision')::bigint<>NEW.plan_revision OR b->>'plan_digest' IS DISTINCT FROM NEW.plan_digest OR b->>'run_id'<>NEW.run_id::text OR COALESCE(b->>'run_revision','') IS DISTINCT FROM NEW.run_revision::text OR b->>'stage_id'<>NEW.stage_id::text OR (b->>'stage_revision')::bigint<>NEW.stage_revision OR b->>'stage_digest'<>NEW.plan_stage_digest OR b->>'target_id' IS DISTINCT FROM COALESCE(NEW.target_id::text,'') OR COALESCE(b->>'target_revision','') IS DISTINCT FROM COALESCE(NEW.target_revision::text,'') OR b->>'target_digest' IS DISTINCT FROM COALESCE(NEW.target_digest,'') THEN RAISE EXCEPTION 'execution confirmation binding scope mismatch'; END IF; END IF; RETURN NEW; END $$`,
		`CREATE TRIGGER core_execution_run_stages_exact_scope_guard BEFORE INSERT OR UPDATE ON core_execution_run_stages FOR EACH ROW EXECUTE FUNCTION core_execution_run_stage_exact_scope_guard()`,
		`CREATE OR REPLACE FUNCTION core_execution_confirmation_preview_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.operation_domain LIKE 'execution:v2%' AND (NEW.preview_digest IS NULL OR NEW.binding_json->>'preview_digest' IS DISTINCT FROM NEW.preview_digest OR NEW.binding_json->>'plan_id'='' OR NEW.binding_json->>'run_id'='' OR NEW.binding_json->>'stage_id'='') THEN RAISE EXCEPTION 'execution confirmation preview/binding mismatch'; END IF; RETURN NEW; END $$`,
		`CREATE TRIGGER agent_confirmations_execution_preview_guard BEFORE INSERT OR UPDATE ON agent_confirmations FOR EACH ROW EXECUTE FUNCTION core_execution_confirmation_preview_guard()`,
		`CREATE TRIGGER core_execution_run_stages_task_scope_guard BEFORE INSERT OR UPDATE ON core_execution_run_stages FOR EACH ROW EXECUTE FUNCTION core_execution_run_stage_task_scope_guard()`,
		`CREATE OR REPLACE FUNCTION core_execution_reconciliation_scope_guard() RETURNS trigger LANGUAGE plpgsql AS $$ DECLARE lease core_execution_target_mutation_leases%ROWTYPE; receipt core_execution_receipts%ROWTYPE; BEGIN IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'execution reconciliation resolutions are append-only'; END IF; SELECT * INTO lease FROM core_execution_target_mutation_leases WHERE owner_id=NEW.owner_id AND lease_id=NEW.lease_id; IF NOT FOUND THEN RAISE EXCEPTION 'reconciliation resolution must match uncertain lease and receipt evidence'; END IF; SELECT * INTO receipt FROM core_execution_receipts WHERE owner_id=NEW.owner_id AND run_id=NEW.run_id AND receipt_id=NEW.receipt_id; IF NOT FOUND OR lease.status IS DISTINCT FROM 'uncertain' OR lease.run_id IS DISTINCT FROM NEW.run_id OR lease.stage_id IS DISTINCT FROM NEW.stage_id OR lease.receipt_id IS DISTINCT FROM NEW.receipt_id OR lease.token IS DISTINCT FROM NEW.token OR lease.epoch IS DISTINCT FROM NEW.epoch OR lease.provider_operation_id IS DISTINCT FROM NEW.provider_operation_id OR receipt.status IS DISTINCT FROM 'uncertain' OR receipt.provider_operation_id IS DISTINCT FROM NEW.provider_operation_id OR receipt.request_digest IS DISTINCT FROM NEW.request_digest THEN RAISE EXCEPTION 'reconciliation resolution must match uncertain lease and receipt evidence'; END IF; RETURN NEW; END $$`,
		`CREATE TRIGGER core_execution_reconciliation_scope_guard BEFORE INSERT OR UPDATE OR DELETE ON core_execution_reconciliation_resolutions FOR EACH ROW EXECUTE FUNCTION core_execution_reconciliation_scope_guard()`,
	}); err != nil {
		return err
	}
	return execMigrationStatements(ctx, txn, []string{
		`CREATE OR REPLACE FUNCTION core_execution_run_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ DECLARE reconciled BOOLEAN; BEGIN SELECT EXISTS(SELECT 1 FROM core_execution_reconciliation_resolutions r WHERE r.owner_id=OLD.owner_id AND r.run_id=OLD.run_id AND r.stage_id=OLD.current_stage_id AND (NEW.status='running' OR r.outcome=NEW.status)) INTO reconciled; IF TG_OP='DELETE' OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.project_id IS DISTINCT FROM OLD.project_id OR NEW.plan_id IS DISTINCT FROM OLD.plan_id OR NEW.plan_revision IS DISTINCT FROM OLD.plan_revision OR NEW.rollback_of_run_id IS DISTINCT FROM OLD.rollback_of_run_id OR NEW.deployment_id IS DISTINCT FROM OLD.deployment_id OR NEW.operation IS DISTINCT FROM OLD.operation OR NEW.purpose IS DISTINCT FROM OLD.purpose OR NEW.trigger_kind IS DISTINCT FROM OLD.trigger_kind OR NEW.plan_digest IS DISTINCT FROM OLD.plan_digest OR NEW.schema_version IS DISTINCT FROM OLD.schema_version OR NEW.run_digest IS DISTINCT FROM OLD.run_digest OR NEW.created_at IS DISTINCT FROM OLD.created_at OR NEW.revision<>OLD.revision+1 OR (OLD.started_at IS NOT NULL AND NEW.started_at IS DISTINCT FROM OLD.started_at) OR (OLD.completed_at IS NOT NULL AND NEW.completed_at IS DISTINCT FROM OLD.completed_at AND NOT reconciled) OR (OLD.terminal_reason<>'' AND NEW.terminal_reason IS DISTINCT FROM OLD.terminal_reason AND NOT reconciled) OR (OLD.status IN ('running','succeeded','failed','uncertain','canceled','rejected','expired') AND NEW.status IN ('pending','waiting_user','queued')) OR (OLD.status='uncertain' AND NEW.status IS DISTINCT FROM OLD.status AND NOT reconciled) OR (OLD.status IN ('succeeded','failed','canceled','rejected','expired') AND NEW.status IS DISTINCT FROM OLD.status) THEN RAISE EXCEPTION 'execution run identity/lifecycle is immutable or revision is not consecutive'; END IF; RETURN NEW; END $$`,
		`CREATE OR REPLACE FUNCTION core_execution_run_stage_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ DECLARE reconciled BOOLEAN; BEGIN SELECT EXISTS(SELECT 1 FROM core_execution_reconciliation_resolutions r WHERE r.owner_id=OLD.owner_id AND r.run_id=OLD.run_id AND r.stage_id=OLD.stage_id AND r.outcome=NEW.status) INTO reconciled; IF TG_OP='DELETE' OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.stage_id IS DISTINCT FROM OLD.stage_id OR NEW.project_id IS DISTINCT FROM OLD.project_id OR NEW.plan_id IS DISTINCT FROM OLD.plan_id OR NEW.plan_revision IS DISTINCT FROM OLD.plan_revision OR NEW.plan_digest IS DISTINCT FROM OLD.plan_digest OR NEW.run_revision IS DISTINCT FROM OLD.run_revision OR NEW.plan_stage_key IS DISTINCT FROM OLD.plan_stage_key OR NEW.stage_revision IS DISTINCT FROM OLD.stage_revision OR NEW.plan_stage_digest IS DISTINCT FROM OLD.plan_stage_digest OR NEW.created_at IS DISTINCT FROM OLD.created_at OR (OLD.started_at IS NOT NULL AND NEW.started_at IS DISTINCT FROM OLD.started_at) OR (OLD.completed_at IS NOT NULL AND NEW.completed_at IS DISTINCT FROM OLD.completed_at) OR (OLD.status IN ('running','succeeded','failed','uncertain','skipped','canceled','rejected','expired') AND NEW.status IN ('blocked','waiting_user','queued')) OR (OLD.status='uncertain' AND NEW.status IS DISTINCT FROM OLD.status AND NOT reconciled) OR (OLD.status IN ('succeeded','failed','skipped','canceled','rejected','expired') AND NEW.status IS DISTINCT FROM OLD.status) THEN RAISE EXCEPTION 'execution run stage identity/state is immutable'; END IF; RETURN NEW; END $$`,
		`CREATE OR REPLACE FUNCTION core_execution_step_attempt_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ DECLARE reconciled BOOLEAN; BEGIN SELECT EXISTS(SELECT 1 FROM core_execution_reconciliation_resolutions r JOIN core_execution_receipts receipt ON receipt.owner_id=r.owner_id AND receipt.run_id=r.run_id AND receipt.receipt_id=r.receipt_id WHERE r.owner_id=OLD.owner_id AND r.run_id=OLD.run_id AND r.stage_id=OLD.stage_id AND receipt.attempt_id=OLD.attempt_id AND r.outcome=NEW.status) INTO reconciled; IF TG_OP='DELETE' OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.attempt_id IS DISTINCT FROM OLD.attempt_id OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.stage_id IS DISTINCT FROM OLD.stage_id OR NEW.project_id IS DISTINCT FROM OLD.project_id OR NEW.plan_id IS DISTINCT FROM OLD.plan_id OR NEW.plan_revision IS DISTINCT FROM OLD.plan_revision OR NEW.plan_stage_key IS DISTINCT FROM OLD.plan_stage_key OR NEW.step_key IS DISTINCT FROM OLD.step_key OR NEW.step_revision IS DISTINCT FROM OLD.step_revision OR NEW.step_digest IS DISTINCT FROM OLD.step_digest OR NEW.revision<>OLD.revision+1 OR (OLD.status IN ('running','succeeded','failed','uncertain','canceled') AND NEW.status IN ('pending','queued')) OR (OLD.status='uncertain' AND NEW.status IS DISTINCT FROM OLD.status AND NOT reconciled) OR (OLD.status IN ('succeeded','failed','canceled') AND NEW.status IS DISTINCT FROM OLD.status) THEN RAISE EXCEPTION 'execution step attempt identity/state is immutable'; END IF; RETURN NEW; END $$`,
		`CREATE OR REPLACE FUNCTION core_execution_receipt_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ DECLARE reconciled BOOLEAN; BEGIN SELECT EXISTS(SELECT 1 FROM core_execution_reconciliation_resolutions r WHERE r.owner_id=OLD.owner_id AND r.run_id=OLD.run_id AND r.receipt_id=OLD.receipt_id AND r.outcome=NEW.status) INTO reconciled; IF TG_OP='DELETE' OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.receipt_id IS DISTINCT FROM OLD.receipt_id OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.attempt_id IS DISTINCT FROM OLD.attempt_id OR NEW.schema_version IS DISTINCT FROM OLD.schema_version OR NEW.created_at IS DISTINCT FROM OLD.created_at OR NEW.request_digest IS DISTINCT FROM OLD.request_digest OR NEW.fence_digest IS DISTINCT FROM OLD.fence_digest OR (OLD.provider_operation_id<>'' AND NEW.provider_operation_id IS DISTINCT FROM OLD.provider_operation_id) OR (OLD.command_id<>'' AND NEW.command_id IS DISTINCT FROM OLD.command_id) OR NEW.idempotency_digest IS DISTINCT FROM OLD.idempotency_digest OR (OLD.status='accepted' AND NEW.status NOT IN ('accepted','running','succeeded','failed','uncertain','canceled')) OR (OLD.status='running' AND NEW.status NOT IN ('running','succeeded','failed','uncertain','canceled')) OR (NEW.status IN ('succeeded','failed','canceled') AND (NEW.response_digest IS NULL OR (NEW.provider_operation_id='' AND NEW.command_id=''))) OR (NEW.status='uncertain' AND NEW.response_digest IS NULL) OR (OLD.status='uncertain' AND NEW.status IS DISTINCT FROM OLD.status AND (NOT reconciled OR NEW.revision<>OLD.revision+1 OR NEW.snapshot_json IS DISTINCT FROM OLD.snapshot_json)) OR (OLD.status IN ('succeeded','failed','canceled') AND (NEW.status IS DISTINCT FROM OLD.status OR NEW.provider_operation_id IS DISTINCT FROM OLD.provider_operation_id OR NEW.command_id IS DISTINCT FROM OLD.command_id OR NEW.response_digest IS DISTINCT FROM OLD.response_digest OR NEW.snapshot_json IS DISTINCT FROM OLD.snapshot_json OR NEW.revision IS DISTINCT FROM OLD.revision)) THEN RAISE EXCEPTION 'execution receipt pin is immutable or regressive'; END IF; RETURN NEW; END $$`,
		`CREATE OR REPLACE FUNCTION core_execution_dispatch_intent_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ DECLARE reconciled BOOLEAN; BEGIN SELECT EXISTS(SELECT 1 FROM core_execution_reconciliation_resolutions r WHERE r.owner_id=OLD.owner_id AND r.run_id=OLD.run_id AND r.stage_id=OLD.stage_id AND r.receipt_id=OLD.receipt_id AND r.outcome=NEW.status) INTO reconciled; IF TG_OP='DELETE' OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.intent_id IS DISTINCT FROM OLD.intent_id OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.stage_id IS DISTINCT FROM OLD.stage_id OR NEW.attempt_id IS DISTINCT FROM OLD.attempt_id OR NEW.receipt_id IS DISTINCT FROM OLD.receipt_id OR NEW.task_id IS DISTINCT FROM OLD.task_id OR NEW.task_lease_epoch IS DISTINCT FROM OLD.task_lease_epoch OR NEW.target_id IS DISTINCT FROM OLD.target_id OR NEW.target_revision IS DISTINCT FROM OLD.target_revision OR NEW.target_digest IS DISTINCT FROM OLD.target_digest OR NEW.plan_id IS DISTINCT FROM OLD.plan_id OR NEW.plan_revision IS DISTINCT FROM OLD.plan_revision OR NEW.plan_digest IS DISTINCT FROM OLD.plan_digest OR NEW.stage_revision IS DISTINCT FROM OLD.stage_revision OR NEW.stage_digest IS DISTINCT FROM OLD.stage_digest OR NEW.step_key IS DISTINCT FROM OLD.step_key OR NEW.step_set IS DISTINCT FROM OLD.step_set OR NEW.step_revision IS DISTINCT FROM OLD.step_revision OR NEW.step_digest IS DISTINCT FROM OLD.step_digest OR NEW.attempt_no IS DISTINCT FROM OLD.attempt_no OR NEW.lease_id IS DISTINCT FROM OLD.lease_id OR NEW.lease_token IS DISTINCT FROM OLD.lease_token OR NEW.lease_epoch IS DISTINCT FROM OLD.lease_epoch OR NEW.request_digest IS DISTINCT FROM OLD.request_digest OR NEW.fence_digest IS DISTINCT FROM OLD.fence_digest OR NEW.schema_version IS DISTINCT FROM OLD.schema_version OR NEW.snapshot_json IS DISTINCT FROM OLD.snapshot_json OR NEW.created_at IS DISTINCT FROM OLD.created_at OR NEW.revision<>OLD.revision+1 OR (OLD.status='intent' AND NEW.status NOT IN ('intent','accepted','uncertain','succeeded','failed','canceled')) OR (OLD.status='accepted' AND NEW.status NOT IN ('accepted','uncertain','succeeded','failed','canceled')) OR (OLD.status='uncertain' AND NEW.status IS DISTINCT FROM OLD.status AND NOT reconciled) OR (OLD.status IN ('succeeded','failed','canceled') AND NEW.status IS DISTINCT FROM OLD.status) THEN RAISE EXCEPTION 'execution dispatch intent identity/state is immutable or regressive'; END IF; RETURN NEW; END $$`,
		`CREATE OR REPLACE FUNCTION core_execution_receipt_terminal_evidence_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF TG_OP<>'UPDATE' THEN RETURN NEW; END IF; IF OLD.status IN ('succeeded','failed','canceled') AND (NEW.provider_operation_id IS DISTINCT FROM OLD.provider_operation_id OR NEW.command_id IS DISTINCT FROM OLD.command_id OR NEW.request_digest IS DISTINCT FROM OLD.request_digest OR NEW.fence_digest IS DISTINCT FROM OLD.fence_digest OR NEW.response_digest IS DISTINCT FROM OLD.response_digest) THEN RAISE EXCEPTION 'execution receipt terminal evidence is immutable'; END IF; IF OLD.status='uncertain' AND (NEW.provider_operation_id IS DISTINCT FROM OLD.provider_operation_id OR NEW.command_id IS DISTINCT FROM OLD.command_id OR NEW.request_digest IS DISTINCT FROM OLD.request_digest OR NEW.fence_digest IS DISTINCT FROM OLD.fence_digest) THEN RAISE EXCEPTION 'execution receipt terminal evidence is immutable during reconciliation'; END IF; IF OLD.status IN ('accepted','running') AND NEW.status=OLD.status AND (NEW.provider_operation_id IS DISTINCT FROM OLD.provider_operation_id OR NEW.command_id IS DISTINCT FROM OLD.command_id OR NEW.response_digest IS DISTINCT FROM OLD.response_digest) THEN RAISE EXCEPTION 'execution receipt evidence may only be filled while advancing or terminalizing'; END IF; IF NEW.status='uncertain' AND OLD.status IN ('accepted','running') AND ((OLD.provider_operation_id<>'' AND NEW.provider_operation_id IS DISTINCT FROM OLD.provider_operation_id) OR (OLD.command_id<>'' AND NEW.command_id IS DISTINCT FROM OLD.command_id)) THEN RAISE EXCEPTION 'execution receipt uncertain evidence may only fill an empty provider identifier'; END IF; RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_receipts_terminal_evidence_guard ON core_execution_receipts`, `DROP TRIGGER IF EXISTS core_execution_receipt_terminal_evidence_guard ON core_execution_receipts`, `CREATE TRIGGER core_execution_receipt_terminal_evidence_guard BEFORE UPDATE ON core_execution_receipts FOR EACH ROW EXECUTE FUNCTION core_execution_receipt_terminal_evidence_guard()`,
		`CREATE OR REPLACE FUNCTION core_execution_deployment_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF TG_OP='DELETE' THEN RAISE EXCEPTION 'execution deployments cannot be deleted'; END IF; IF NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.deployment_id IS DISTINCT FROM OLD.deployment_id OR NEW.project_id IS DISTINCT FROM OLD.project_id OR NEW.created_at IS DISTINCT FROM OLD.created_at OR NEW.revision<>OLD.revision+1 OR NEW.state NOT IN ('pending','waiting_user','queued','running','succeeded','failed','uncertain','canceled','rejected','expired') OR (NEW.current_run_id=OLD.current_run_id AND ((OLD.state='pending' AND NEW.state NOT IN ('pending','waiting_user','queued','running','succeeded','failed','uncertain','canceled','rejected','expired')) OR (OLD.state='waiting_user' AND NEW.state NOT IN ('waiting_user','queued','running','succeeded','failed','uncertain','canceled','rejected','expired')) OR (OLD.state='queued' AND NEW.state NOT IN ('queued','running','succeeded','failed','uncertain','canceled','rejected','expired')) OR (OLD.state='running' AND NEW.state NOT IN ('running','succeeded','failed','uncertain','canceled')) OR (OLD.state IN ('succeeded','failed','uncertain','canceled','rejected','expired') AND NEW.state IS DISTINCT FROM OLD.state))) THEN RAISE EXCEPTION 'execution deployment identity/state is immutable or revision is not consecutive'; END IF; RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_deployments_immutable ON core_execution_deployments`, `CREATE TRIGGER core_execution_deployments_immutable BEFORE UPDATE OR DELETE ON core_execution_deployments FOR EACH ROW EXECUTE FUNCTION core_execution_deployment_immutable()`,
		`CREATE OR REPLACE FUNCTION core_execution_deployment_event_append_only() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'execution deployment events are append-only'; END $$`,
		`DROP TRIGGER IF EXISTS core_execution_deployment_events_append_only ON core_execution_deployment_events`, `CREATE TRIGGER core_execution_deployment_events_append_only BEFORE UPDATE OR DELETE ON core_execution_deployment_events FOR EACH ROW EXECUTE FUNCTION core_execution_deployment_event_append_only()`,
		`CREATE OR REPLACE FUNCTION core_execution_deployment_scope_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF TG_TABLE_NAME='core_execution_deployments' THEN IF NOT EXISTS (SELECT 1 FROM core_execution_runs WHERE owner_id=NEW.owner_id AND run_id=NEW.current_run_id AND project_id=NEW.project_id AND purpose='service' AND deployment_id=NEW.deployment_id) THEN RAISE EXCEPTION 'execution deployment requires reciprocal service run'; END IF; ELSE IF NEW.purpose='job' AND NEW.deployment_id IS NOT NULL THEN RAISE EXCEPTION 'job run cannot bind deployment'; END IF; IF NEW.purpose='service' AND NOT EXISTS (SELECT 1 FROM core_execution_deployments WHERE owner_id=NEW.owner_id AND deployment_id=NEW.deployment_id AND project_id=NEW.project_id) THEN RAISE EXCEPTION 'service run requires deployment'; END IF; END IF; RETURN NEW; END $$`,
		`CREATE CONSTRAINT TRIGGER core_execution_deployments_reciprocal_guard AFTER INSERT OR UPDATE ON core_execution_deployments DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION core_execution_deployment_scope_guard()`,
		`CREATE CONSTRAINT TRIGGER core_execution_runs_deployment_reciprocal_guard AFTER INSERT OR UPDATE ON core_execution_runs DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION core_execution_deployment_scope_guard()`,
		`CREATE OR REPLACE FUNCTION core_execution_service_binding_scope_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF TG_OP='DELETE' THEN RAISE EXCEPTION 'service bindings are immutable'; END IF; IF NOT EXISTS (SELECT 1 FROM core_execution_deployments WHERE owner_id=NEW.owner_id AND deployment_id=NEW.deployment_id AND project_id=NEW.project_id AND current_run_id=NEW.run_id) OR NOT EXISTS (SELECT 1 FROM core_execution_runs WHERE owner_id=NEW.owner_id AND run_id=NEW.run_id AND deployment_id=NEW.deployment_id AND project_id=NEW.project_id AND purpose='service') THEN RAISE EXCEPTION 'service binding deployment scope mismatch'; END IF; IF TG_OP='UPDATE' AND (NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.binding_id IS DISTINCT FROM OLD.binding_id OR NEW.deployment_id IS DISTINCT FROM OLD.deployment_id OR NEW.project_id IS DISTINCT FROM OLD.project_id OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.target_id IS DISTINCT FROM OLD.target_id OR NEW.target_revision IS DISTINCT FROM OLD.target_revision OR NEW.schema_version IS DISTINCT FROM OLD.schema_version OR NEW.created_at IS DISTINCT FROM OLD.created_at OR NEW.revision<>OLD.revision+1) THEN RAISE EXCEPTION 'service binding identity or revision is immutable'; END IF; IF NEW.snapshot_json->>'owner_id' IS DISTINCT FROM NEW.owner_id OR NEW.snapshot_json->>'binding_id' IS DISTINCT FROM NEW.binding_id::text OR NEW.snapshot_json->>'deployment_id' IS DISTINCT FROM NEW.deployment_id::text OR NEW.snapshot_json->>'project_id' IS DISTINCT FROM NEW.project_id::text OR NEW.snapshot_json->>'run_id' IS DISTINCT FROM NEW.run_id::text OR NEW.snapshot_json->>'target_id' IS DISTINCT FROM COALESCE(NEW.target_id::text,'') OR NEW.snapshot_json->>'target_revision' IS DISTINCT FROM COALESCE(NEW.target_revision::text,'') OR NEW.snapshot_json->>'target_digest' IS DISTINCT FROM COALESCE((SELECT target_digest FROM core_execution_targets WHERE owner_id=NEW.owner_id AND target_id=NEW.target_id AND target_revision=NEW.target_revision),'') OR NEW.snapshot_json->>'protocol' IS DISTINCT FROM NEW.protocol OR NEW.snapshot_json->>'endpoint' IS DISTINCT FROM NEW.endpoint OR NEW.snapshot_json->>'digest' IS DISTINCT FROM NEW.binding_digest OR COALESCE((NEW.snapshot_json->>'revision')::bigint,0)<>NEW.revision THEN RAISE EXCEPTION 'service binding snapshot mismatch'; END IF; RETURN NEW; END $$`,
		`CREATE TRIGGER core_execution_service_bindings_scope_guard BEFORE INSERT OR UPDATE OR DELETE ON core_execution_service_bindings FOR EACH ROW EXECUTE FUNCTION core_execution_service_binding_scope_guard()`,
	})
}

// backfillLegacyChannelFavorites preserves the owner-local channel-post
// favorites recorded before favorites became Matrix-projected reactions. A
// legacy row belongs to this portal's owner, so the reaction key remains
// independently addressable per Matrix user. Existing projected reactions win.
func backfillLegacyChannelFavorites(ctx context.Context, txn *sql.Tx) error {
	for _, table := range []string{"p2p_favorites", "p2p_channel_posts", "p2p_portal", "p2p_reactions"} {
		exists, err := productTableExists(ctx, txn, table)
		if err != nil || !exists {
			return err
		}
	}
	_, err := txn.ExecContext(ctx, `
		INSERT INTO p2p_reactions (
			target_type, target_id, channel_id, post_id, comment_id, reaction, user_id, active, created_at
		)
		SELECT 'post', post.post_id, post.channel_id, post.post_id, '', 'favorite', portal.owner_mxid, 1, favorite.created_at
		FROM p2p_favorites AS favorite
		JOIN p2p_channel_posts AS post
			ON post.event_id = favorite.event_id
			AND (favorite.room_id = '' OR favorite.room_id = post.room_id)
		JOIN p2p_portal AS portal ON portal.id = 'owner'
		WHERE portal.owner_mxid <> ''
		ON CONFLICT (target_type, target_id, reaction, user_id) DO NOTHING
	`)
	return err
}

func execMigrationStatements(ctx context.Context, txn *sql.Tx, statements []string) error {
	for _, statement := range statements {
		if _, err := txn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func execMigrationDDL(ctx context.Context, txn *sql.Tx, ddl string) error {
	statements := make([]string, 0)
	for _, statement := range strings.Split(ddl, ";") {
		if statement = strings.TrimSpace(statement); statement != "" {
			statements = append(statements, statement)
		}
	}
	return execMigrationStatements(ctx, txn, statements)
}
