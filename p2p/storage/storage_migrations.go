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
					origin_server_ts BIGINT NOT NULL,
					comment_count BIGINT NOT NULL
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_channel_posts_channel_idx ON p2p_channel_posts(channel_id, origin_server_ts)`,
				`CREATE INDEX IF NOT EXISTS p2p_channel_posts_event_idx ON p2p_channel_posts(event_id)`,
				`CREATE INDEX IF NOT EXISTS p2p_channel_posts_author_idx ON p2p_channel_posts(author_mxid, origin_server_ts)`,
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
		Version: "p2p: durable agent core turns v78",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`CREATE TABLE IF NOT EXISTS p2p_agent_core_turns (
					owner_id TEXT NOT NULL CHECK (owner_id <> ''),
					client_turn_id TEXT NOT NULL CHECK (client_turn_id <> ''),
					core_turn_id TEXT NOT NULL DEFAULT '',
					core_profile_id TEXT NOT NULL DEFAULT '',
					conversation_id TEXT NOT NULL DEFAULT '',
					request_digest BYTEA NOT NULL CHECK (octet_length(request_digest) = 32),
					status TEXT NOT NULL CHECK (status <> ''),
					last_sequence BIGINT NOT NULL DEFAULT 0 CHECK (last_sequence >= 0),
					core_revision BIGINT NOT NULL DEFAULT 0,
					model_profile_revision BIGINT NOT NULL DEFAULT 0,
					last_event_kind TEXT NOT NULL DEFAULT '',
					terminal_code TEXT NOT NULL DEFAULT '',
					terminal_summary TEXT NOT NULL DEFAULT '',
					created_at TIMESTAMPTZ NOT NULL,
					updated_at TIMESTAMPTZ NOT NULL,
					PRIMARY KEY (owner_id, client_turn_id)
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_agent_core_turns_owner_conversation_idx ON p2p_agent_core_turns(owner_id, conversation_id, created_at, client_turn_id)`,
				`ALTER TABLE p2p_agent_core_turns ADD COLUMN IF NOT EXISTS core_revision BIGINT NOT NULL DEFAULT 0`,
				`ALTER TABLE p2p_agent_core_turns ADD COLUMN IF NOT EXISTS core_profile_id TEXT NOT NULL DEFAULT ''`,
				`ALTER TABLE p2p_agent_core_turns ADD COLUMN IF NOT EXISTS model_profile_revision BIGINT NOT NULL DEFAULT 0`,
				`ALTER TABLE p2p_agent_core_turns ADD COLUMN IF NOT EXISTS last_event_kind TEXT NOT NULL DEFAULT ''`,
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
		Version: "p2p: encrypted server model profiles v79",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`CREATE TABLE IF NOT EXISTS p2p_agent_model_profiles (
					owner_id TEXT NOT NULL CHECK (owner_id <> ''),
					profile_id TEXT NOT NULL CHECK (profile_id <> ''),
					client_profile_id TEXT NOT NULL CHECK (client_profile_id <> ''),
					display_name TEXT NOT NULL DEFAULT '',
					provider TEXT NOT NULL,
					base_url TEXT NOT NULL DEFAULT '',
					model TEXT NOT NULL DEFAULT '',
					system_prompt TEXT NOT NULL DEFAULT '',
					temperature DOUBLE PRECISION,
					top_p DOUBLE PRECISION,
					max_output_tokens BIGINT NOT NULL DEFAULT 0,
					context_window BIGINT NOT NULL DEFAULT 0,
					reasoning_effort TEXT NOT NULL DEFAULT '',
					model_kind TEXT NOT NULL DEFAULT 'conversation',
					input_modalities JSONB NOT NULL DEFAULT '["text"]'::jsonb,
					provider_config JSONB NOT NULL DEFAULT '{}'::jsonb,
					revision BIGINT NOT NULL CHECK (revision > 0),
					api_key_version BIGINT NOT NULL DEFAULT 1,
					api_key_nonce BYTEA NOT NULL DEFAULT ''::bytea,
					api_key_ciphertext BYTEA NOT NULL DEFAULT ''::bytea,
					credential_version BIGINT NOT NULL DEFAULT 0,
					created_at TIMESTAMPTZ NOT NULL,
					updated_at TIMESTAMPTZ NOT NULL,
					PRIMARY KEY (owner_id, profile_id),
					UNIQUE (owner_id, client_profile_id)
				)`,
				`CREATE TABLE IF NOT EXISTS p2p_agent_model_profile_credentials (
					owner_id TEXT NOT NULL,
					profile_id TEXT NOT NULL,
					credential_version BIGINT NOT NULL CHECK (credential_version > 0),
					provider TEXT NOT NULL,
					api_key_nonce BYTEA NOT NULL,
					api_key_ciphertext BYTEA NOT NULL,
					created_at TIMESTAMPTZ NOT NULL,
					PRIMARY KEY (owner_id, profile_id, credential_version),
					FOREIGN KEY (owner_id, profile_id) REFERENCES p2p_agent_model_profiles(owner_id, profile_id) ON DELETE CASCADE
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_agent_model_profiles_owner_idx ON p2p_agent_model_profiles(owner_id, client_profile_id, profile_id)`,
				`CREATE TABLE IF NOT EXISTS p2p_agent_model_profile_defaults (
					owner_id TEXT PRIMARY KEY NOT NULL,
					profile_id TEXT,
					client_profile_id TEXT NOT NULL,
					embedding_profile_id TEXT,
					embedding_client_profile_id TEXT NOT NULL DEFAULT '',
					speech_profile_id TEXT,
					speech_client_profile_id TEXT NOT NULL DEFAULT ''
				)`,
				`CREATE TABLE IF NOT EXISTS p2p_agent_model_profile_syncs (
					owner_id TEXT NOT NULL,
					idempotency_key TEXT NOT NULL,
					request_digest BYTEA NOT NULL CHECK (octet_length(request_digest) = 32),
					response_json JSONB NOT NULL,
					created_at TIMESTAMPTZ NOT NULL,
					PRIMARY KEY (owner_id, idempotency_key)
				)`,
				`CREATE TABLE IF NOT EXISTS p2p_agent_model_profile_deletes (
					owner_id TEXT NOT NULL,
					idempotency_key TEXT NOT NULL,
					profile_id TEXT NOT NULL,
					created_at TIMESTAMPTZ NOT NULL,
					PRIMARY KEY (owner_id, idempotency_key)
				)`,
				`ALTER TABLE p2p_agent_model_profiles ADD COLUMN IF NOT EXISTS credential_version BIGINT NOT NULL DEFAULT 0`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: immutable model profile revisions v80",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`ALTER TABLE p2p_agent_model_profiles ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ`,
				`ALTER TABLE p2p_agent_model_profile_credentials DROP CONSTRAINT IF EXISTS p2p_agent_model_profile_credentials_owner_id_profile_id_fkey`,
				`CREATE TABLE IF NOT EXISTS p2p_agent_model_profile_revisions (
					owner_id TEXT NOT NULL,
					profile_id TEXT NOT NULL,
					profile_revision BIGINT NOT NULL CHECK (profile_revision > 0),
					client_profile_id TEXT NOT NULL,
					display_name TEXT NOT NULL DEFAULT '',
					provider TEXT NOT NULL,
					base_url TEXT NOT NULL DEFAULT '',
					model TEXT NOT NULL DEFAULT '',
					system_prompt TEXT NOT NULL DEFAULT '',
					temperature DOUBLE PRECISION,
					top_p DOUBLE PRECISION,
					max_output_tokens BIGINT NOT NULL DEFAULT 0,
					context_window BIGINT NOT NULL DEFAULT 0,
					reasoning_effort TEXT NOT NULL DEFAULT '',
					model_kind TEXT NOT NULL DEFAULT 'conversation',
					input_modalities JSONB NOT NULL DEFAULT '["text"]'::jsonb,
					provider_config JSONB NOT NULL DEFAULT '{}'::jsonb,
					credential_version BIGINT NOT NULL DEFAULT 0,
					deleted_at TIMESTAMPTZ,
					created_at TIMESTAMPTZ NOT NULL,
					PRIMARY KEY (owner_id, profile_id, profile_revision)
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_agent_model_profile_revisions_lookup_idx ON p2p_agent_model_profile_revisions(owner_id, profile_id, profile_revision)`,
				`INSERT INTO p2p_agent_model_profile_revisions(owner_id,profile_id,profile_revision,client_profile_id,display_name,provider,base_url,model,system_prompt,temperature,top_p,max_output_tokens,context_window,reasoning_effort,credential_version,deleted_at,created_at) SELECT owner_id,profile_id,revision,client_profile_id,display_name,provider,base_url,model,system_prompt,temperature,top_p,max_output_tokens,context_window,reasoning_effort,credential_version,deleted_at,updated_at FROM p2p_agent_model_profiles ON CONFLICT DO NOTHING`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: model profile credential integrity and delete idempotency v81",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`DELETE FROM p2p_agent_model_profile_credentials c WHERE NOT EXISTS (SELECT 1 FROM p2p_agent_model_profiles p WHERE p.owner_id=c.owner_id AND p.profile_id=c.profile_id)`,
				`ALTER TABLE p2p_agent_model_profile_credentials ADD CONSTRAINT p2p_agent_model_profile_credentials_owner_id_profile_id_fkey FOREIGN KEY (owner_id, profile_id) REFERENCES p2p_agent_model_profiles(owner_id, profile_id) ON DELETE RESTRICT`,
				`ALTER TABLE p2p_agent_model_profile_deletes ADD COLUMN IF NOT EXISTS request_digest BYTEA`,
				`ALTER TABLE p2p_agent_model_profile_deletes ADD COLUMN IF NOT EXISTS response_json JSONB NOT NULL DEFAULT '{}'::jsonb`,
				`UPDATE p2p_agent_model_profile_deletes SET response_json=jsonb_build_object('deleted',true,'profile_id',profile_id) WHERE response_json='{}'::jsonb`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: pinned native agent turn profiles v82",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`ALTER TABLE p2p_native_agent_turns ADD COLUMN IF NOT EXISTS model_profile_id TEXT NOT NULL DEFAULT ''`,
				`ALTER TABLE p2p_native_agent_turns ADD COLUMN IF NOT EXISTS model_profile_revision BIGINT NOT NULL DEFAULT 0`,
				`ALTER TABLE p2p_native_agent_turns ADD COLUMN IF NOT EXISTS credential_version BIGINT NOT NULL DEFAULT 0`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: embedded schedules v83",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`CREATE TABLE IF NOT EXISTS p2p_agent_schedules (schedule_id TEXT NOT NULL, owner_id TEXT NOT NULL, name TEXT NOT NULL, prompt TEXT NOT NULL, trigger_kind TEXT NOT NULL, trigger_value TEXT NOT NULL, timezone TEXT NOT NULL, skip_if_running BOOLEAN NOT NULL DEFAULT FALSE, status TEXT NOT NULL, revision BIGINT NOT NULL DEFAULT 1, model_profile_id TEXT NOT NULL, model_profile_revision BIGINT NOT NULL, credential_version BIGINT NOT NULL, next_run_at TIMESTAMPTZ, latest_run_at TIMESTAMPTZ, lease_owner TEXT NOT NULL DEFAULT '', lease_until TIMESTAMPTZ, lease_epoch BIGINT NOT NULL DEFAULT 0, idempotency_key TEXT NOT NULL DEFAULT '', deleted_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, PRIMARY KEY(owner_id,schedule_id))`,
				`CREATE UNIQUE INDEX IF NOT EXISTS p2p_agent_schedules_idem_idx ON p2p_agent_schedules(owner_id,idempotency_key) WHERE idempotency_key <> ''`,
				`CREATE INDEX IF NOT EXISTS p2p_agent_schedules_due_idx ON p2p_agent_schedules(status,next_run_at) WHERE deleted_at IS NULL`,
				`CREATE TABLE IF NOT EXISTS p2p_agent_schedule_runs (run_id TEXT PRIMARY KEY NOT NULL, schedule_id TEXT NOT NULL, owner_id TEXT NOT NULL, status TEXT NOT NULL, scheduled_for TIMESTAMPTZ NOT NULL, started_at TIMESTAMPTZ, finished_at TIMESTAMPTZ, result TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '', lease_epoch BIGINT NOT NULL DEFAULT 0, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
				`CREATE INDEX IF NOT EXISTS p2p_agent_schedule_runs_owner_idx ON p2p_agent_schedule_runs(owner_id,schedule_id,run_id)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: schedule mutation receipts and CAS v84",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`CREATE TABLE IF NOT EXISTS p2p_agent_schedule_mutations (owner_id TEXT NOT NULL, action TEXT NOT NULL, idempotency_key TEXT NOT NULL, request_digest BYTEA NOT NULL CHECK (octet_length(request_digest)=32), response_json JSONB NOT NULL, created_at TIMESTAMPTZ NOT NULL, PRIMARY KEY(owner_id,action,idempotency_key))`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: embedded schedule occurrence fencing v84",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`CREATE UNIQUE INDEX IF NOT EXISTS p2p_agent_schedule_runs_occurrence_idx ON p2p_agent_schedule_runs(owner_id,schedule_id,scheduled_for)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: native schedule confirmations v85",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`CREATE TABLE IF NOT EXISTS p2p_agent_schedule_confirmations (
					confirmation_id TEXT NOT NULL, owner_id TEXT NOT NULL, conversation_id TEXT NOT NULL,
					action TEXT NOT NULL, params_json JSONB NOT NULL, request_digest BYTEA NOT NULL CHECK (octet_length(request_digest)=32),
					idempotency_key TEXT NOT NULL, summary TEXT NOT NULL, approval_code TEXT NOT NULL,
					status TEXT NOT NULL CHECK (status IN ('pending','executing','completed','failed','expired','replaced')),
					revision BIGINT NOT NULL DEFAULT 1, expires_at TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL,
					updated_at TIMESTAMPTZ NOT NULL, result_json JSONB NOT NULL DEFAULT '{}'::jsonb, error_text TEXT NOT NULL DEFAULT '',
					PRIMARY KEY(owner_id,conversation_id,confirmation_id)
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_agent_schedule_confirmations_pending_idx ON p2p_agent_schedule_confirmations(owner_id,conversation_id,status,updated_at)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: native schedule confirmation pending uniqueness v85b", Up: func(ctx context.Context, txn *sql.Tx) error {
		return execMigrationStatements(ctx, txn, []string{`UPDATE p2p_agent_schedule_confirmations c SET status='replaced',revision=revision+1,updated_at=NOW() WHERE status IN ('pending','executing') AND EXISTS (SELECT 1 FROM p2p_agent_schedule_confirmations newer WHERE newer.owner_id=c.owner_id AND newer.conversation_id=c.conversation_id AND newer.status IN ('pending','executing') AND (newer.updated_at>c.updated_at OR (newer.updated_at=c.updated_at AND newer.confirmation_id>c.confirmation_id)))`, `CREATE UNIQUE INDEX IF NOT EXISTS p2p_agent_schedule_confirmations_active_idx ON p2p_agent_schedule_confirmations(owner_id,conversation_id) WHERE status IN ('pending','executing')`})
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: embedded native agent conversation memory v86", Up: func(ctx context.Context, txn *sql.Tx) error {
		return execMigrationStatements(ctx, txn, []string{
			`CREATE TABLE IF NOT EXISTS p2p_native_agent_conversations (
				owner_id TEXT NOT NULL CHECK (owner_id <> ''), conversation_id TEXT NOT NULL CHECK (char_length(conversation_id) BETWEEN 1 AND 256),
				title TEXT NOT NULL DEFAULT '', active BOOLEAN NOT NULL DEFAULT TRUE, deleted BOOLEAN NOT NULL DEFAULT FALSE,
				revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0), last_message_seq BIGINT NOT NULL DEFAULT 0 CHECK (last_message_seq >= 0),
				summary TEXT NOT NULL DEFAULT '', summary_through_seq BIGINT NOT NULL DEFAULT 0 CHECK (summary_through_seq >= 0),
				created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, deleted_at TIMESTAMPTZ,
				PRIMARY KEY(owner_id, conversation_id)
			)`,
			`CREATE INDEX IF NOT EXISTS p2p_native_agent_conversations_owner_cursor_idx ON p2p_native_agent_conversations(owner_id,updated_at,conversation_id)`,
			`CREATE TABLE IF NOT EXISTS p2p_native_agent_messages (
				owner_id TEXT NOT NULL, conversation_id TEXT NOT NULL, seq BIGINT NOT NULL CHECK (seq > 0), turn_id TEXT NOT NULL DEFAULT '', message_id TEXT NOT NULL,
				role TEXT NOT NULL CHECK (role IN ('user','assistant')), content TEXT NOT NULL, references_json JSONB NOT NULL DEFAULT '[]'::jsonb, created_at TIMESTAMPTZ NOT NULL,
				PRIMARY KEY(owner_id,conversation_id,seq), UNIQUE(owner_id,conversation_id,message_id),
				FOREIGN KEY(owner_id,conversation_id) REFERENCES p2p_native_agent_conversations(owner_id,conversation_id) ON DELETE CASCADE
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS p2p_native_agent_messages_turn_role_idx ON p2p_native_agent_messages(owner_id,conversation_id,turn_id,role) WHERE turn_id <> ''`,
			`CREATE INDEX IF NOT EXISTS p2p_native_agent_messages_cursor_idx ON p2p_native_agent_messages(owner_id,conversation_id,seq DESC,message_id)`,
			`CREATE TABLE IF NOT EXISTS p2p_native_agent_memory_turns (owner_id TEXT NOT NULL, conversation_id TEXT NOT NULL, turn_id TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, PRIMARY KEY(owner_id,conversation_id,turn_id), FOREIGN KEY(owner_id,conversation_id) REFERENCES p2p_native_agent_conversations(owner_id,conversation_id) ON DELETE CASCADE)`,
			`CREATE TABLE IF NOT EXISTS p2p_native_agent_memory_records (owner_id TEXT NOT NULL, memory_id TEXT NOT NULL, title TEXT NOT NULL, content TEXT NOT NULL, tags_json JSONB NOT NULL DEFAULT '[]'::jsonb, request_digest BYTEA NOT NULL CHECK (octet_length(request_digest)=32), created_at TIMESTAMPTZ NOT NULL, PRIMARY KEY(owner_id,memory_id), UNIQUE(owner_id,request_digest))`,
			`CREATE INDEX IF NOT EXISTS p2p_native_agent_memory_records_cursor_idx ON p2p_native_agent_memory_records(owner_id,created_at,memory_id)`,
			`CREATE TABLE IF NOT EXISTS p2p_native_agent_conversation_mutations (owner_id TEXT NOT NULL, action TEXT NOT NULL, idempotency_key TEXT NOT NULL, request_digest BYTEA NOT NULL CHECK (octet_length(request_digest)=32), response_json JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL, PRIMARY KEY(owner_id,action,idempotency_key))`,
		})
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: native agent memory idempotency keys v87", Up: func(ctx context.Context, txn *sql.Tx) error {
		return execMigrationStatements(ctx, txn, []string{
			`ALTER TABLE p2p_native_agent_memory_records DROP CONSTRAINT IF EXISTS p2p_native_agent_memory_records_owner_id_request_digest_key`,
		})
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: role-aware model profiles v89", Up: func(ctx context.Context, txn *sql.Tx) error {
		return execMigrationStatements(ctx, txn, []string{
			`ALTER TABLE p2p_agent_model_profiles ADD COLUMN IF NOT EXISTS model_kind TEXT NOT NULL DEFAULT 'conversation'`,
			`ALTER TABLE p2p_agent_model_profiles ADD COLUMN IF NOT EXISTS input_modalities JSONB NOT NULL DEFAULT '["text"]'::jsonb`,
			`ALTER TABLE p2p_agent_model_profiles ADD COLUMN IF NOT EXISTS provider_config JSONB NOT NULL DEFAULT '{}'::jsonb`,
			`ALTER TABLE p2p_agent_model_profile_revisions ADD COLUMN IF NOT EXISTS model_kind TEXT NOT NULL DEFAULT 'conversation'`,
			`ALTER TABLE p2p_agent_model_profile_revisions ADD COLUMN IF NOT EXISTS input_modalities JSONB NOT NULL DEFAULT '["text"]'::jsonb`,
			`ALTER TABLE p2p_agent_model_profile_revisions ADD COLUMN IF NOT EXISTS provider_config JSONB NOT NULL DEFAULT '{}'::jsonb`,
			`ALTER TABLE p2p_agent_model_profile_defaults ADD COLUMN IF NOT EXISTS embedding_profile_id TEXT`,
			`ALTER TABLE p2p_agent_model_profile_defaults ADD COLUMN IF NOT EXISTS embedding_client_profile_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE p2p_agent_model_profile_defaults ADD COLUMN IF NOT EXISTS speech_profile_id TEXT`,
			`ALTER TABLE p2p_agent_model_profile_defaults ADD COLUMN IF NOT EXISTS speech_client_profile_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE p2p_agent_model_profile_defaults DROP CONSTRAINT IF EXISTS p2p_agent_model_profile_defaults_owner_id_profile_id_fkey`,
			`ALTER TABLE p2p_agent_model_profile_defaults ALTER COLUMN profile_id DROP NOT NULL`,
			`ALTER TABLE p2p_agent_model_profile_defaults ALTER COLUMN embedding_profile_id DROP NOT NULL`,
			`ALTER TABLE p2p_agent_model_profile_defaults ALTER COLUMN speech_profile_id DROP NOT NULL`,
			`UPDATE p2p_agent_model_profile_defaults SET profile_id=NULLIF(profile_id,''),embedding_profile_id=NULLIF(embedding_profile_id,''),speech_profile_id=NULLIF(speech_profile_id,'')`,
			`ALTER TABLE p2p_agent_model_profile_defaults ADD CONSTRAINT p2p_agent_model_profile_defaults_owner_profile_fkey FOREIGN KEY (owner_id, profile_id) REFERENCES p2p_agent_model_profiles(owner_id, profile_id) ON DELETE RESTRICT`,
			`ALTER TABLE p2p_agent_model_profile_defaults ADD CONSTRAINT p2p_agent_model_profile_defaults_owner_embedding_fkey FOREIGN KEY (owner_id, embedding_profile_id) REFERENCES p2p_agent_model_profiles(owner_id, profile_id) ON DELETE RESTRICT`,
			`ALTER TABLE p2p_agent_model_profile_defaults ADD CONSTRAINT p2p_agent_model_profile_defaults_owner_speech_fkey FOREIGN KEY (owner_id, speech_profile_id) REFERENCES p2p_agent_model_profiles(owner_id, profile_id) ON DELETE RESTRICT`,
		})
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: semantic native agent memory vectors v90", Up: func(ctx context.Context, txn *sql.Tx) error {
		return execMigrationStatements(ctx, txn, []string{
			`CREATE TABLE IF NOT EXISTS p2p_native_agent_memory_embeddings (
				owner_id TEXT NOT NULL, memory_id TEXT NOT NULL, profile_id TEXT NOT NULL,
				profile_revision BIGINT NOT NULL CHECK (profile_revision > 0), model TEXT NOT NULL,
				dimension BIGINT NOT NULL CHECK (dimension BETWEEN 1 AND 32768), content_digest BYTEA NOT NULL CHECK (octet_length(content_digest)=32),
				vector DOUBLE PRECISION[] NOT NULL CHECK (COALESCE(array_length(vector,1),0)=dimension), indexed_at TIMESTAMPTZ NOT NULL,
				PRIMARY KEY(owner_id,memory_id),
				FOREIGN KEY(owner_id,memory_id) REFERENCES p2p_native_agent_memory_records(owner_id,memory_id) ON DELETE CASCADE
			)`,
			`CREATE INDEX IF NOT EXISTS p2p_native_agent_memory_embeddings_profile_idx ON p2p_native_agent_memory_embeddings(owner_id,profile_id,profile_revision,model,dimension)`,
		})
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: managed native agent knowledge v91", Up: func(ctx context.Context, txn *sql.Tx) error {
		return execMigrationStatements(ctx, txn, []string{
			`ALTER TABLE p2p_native_agent_memory_records ADD COLUMN IF NOT EXISTS revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0)`,
			`ALTER TABLE p2p_native_agent_memory_records ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`,
			`CREATE INDEX IF NOT EXISTS p2p_native_agent_memory_records_updated_idx ON p2p_native_agent_memory_records(owner_id,updated_at,memory_id)`,
		})
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: native agent knowledge sources v92", Up: func(ctx context.Context, txn *sql.Tx) error {
		return execMigrationStatements(ctx, txn, []string{
			`CREATE TABLE IF NOT EXISTS p2p_native_agent_knowledge_sources (owner_id TEXT NOT NULL, source_id TEXT NOT NULL, kind TEXT NOT NULL, status TEXT NOT NULL, title TEXT NOT NULL DEFAULT '', mime_type TEXT NOT NULL, size BIGINT NOT NULL, total_chunks BIGINT NOT NULL DEFAULT 0, indexed_chunks BIGINT NOT NULL DEFAULT 0, revision BIGINT NOT NULL DEFAULT 1, error_text TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, PRIMARY KEY(owner_id,source_id))`,
			`CREATE INDEX IF NOT EXISTS p2p_native_agent_knowledge_sources_cursor_idx ON p2p_native_agent_knowledge_sources(owner_id,created_at,source_id)`,
			`CREATE TABLE IF NOT EXISTS p2p_native_agent_knowledge_uploads (owner_id TEXT NOT NULL, upload_id TEXT NOT NULL, source_id TEXT NOT NULL, filename TEXT NOT NULL, mime_type TEXT NOT NULL, size BIGINT NOT NULL, received_size BIGINT NOT NULL DEFAULT 0, data BYTEA NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL, PRIMARY KEY(owner_id,upload_id))`,
		})
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: native agent knowledge source chunks v93", Up: func(ctx context.Context, txn *sql.Tx) error {
		return execMigrationStatements(ctx, txn, []string{
			`ALTER TABLE p2p_native_agent_memory_records ADD COLUMN IF NOT EXISTS source_id TEXT`,
			`ALTER TABLE p2p_native_agent_memory_records ADD COLUMN IF NOT EXISTS chunk_ordinal BIGINT`,
			`CREATE INDEX IF NOT EXISTS p2p_native_agent_memory_records_source_idx ON p2p_native_agent_memory_records(owner_id,source_id,chunk_ordinal)`,
		})
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: agent deployment ledger v94", Up: func(ctx context.Context, txn *sql.Tx) error {
		return execMigrationStatements(ctx, txn, []string{
			`CREATE TABLE IF NOT EXISTS p2p_agent_deployments (
				owner_id TEXT NOT NULL, workload_id TEXT NOT NULL, operation_id TEXT NOT NULL DEFAULT '',
				status TEXT NOT NULL, target_kind TEXT NOT NULL DEFAULT '', object_json JSONB NOT NULL DEFAULT '{}'::jsonb,
				actual_json JSONB NOT NULL DEFAULT '{}'::jsonb, quote_json JSONB NOT NULL DEFAULT '{}'::jsonb,
				created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
				PRIMARY KEY(owner_id, workload_id)
			)`,
			`CREATE INDEX IF NOT EXISTS p2p_agent_deployments_owner_updated_idx ON p2p_agent_deployments(owner_id, updated_at DESC, workload_id)`,
			`CREATE INDEX IF NOT EXISTS p2p_agent_deployments_owner_status_idx ON p2p_agent_deployments(owner_id, status, target_kind)`,
			`CREATE TABLE IF NOT EXISTS p2p_agent_deployment_events (
				owner_id TEXT NOT NULL, workload_id TEXT NOT NULL, operation_id TEXT NOT NULL, sequence BIGINT NOT NULL CHECK(sequence > 0),
				event_json JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
				PRIMARY KEY(owner_id, operation_id, sequence)
			)`,
			`CREATE INDEX IF NOT EXISTS p2p_agent_deployment_events_owner_workload_idx ON p2p_agent_deployment_events(owner_id, workload_id, sequence)`,
		})
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: agent deployment ledger operation snapshot v95", Up: func(ctx context.Context, txn *sql.Tx) error {
		return execMigrationStatements(ctx, txn, []string{`ALTER TABLE p2p_agent_deployments ADD COLUMN IF NOT EXISTS operation_json JSONB NOT NULL DEFAULT '{}'::jsonb`, `ALTER TABLE p2p_agent_deployments ADD COLUMN IF NOT EXISTS revision BIGINT NOT NULL DEFAULT 1`, `ALTER TABLE p2p_agent_deployments ADD COLUMN IF NOT EXISTS lease_owner TEXT NOT NULL DEFAULT ''`, `ALTER TABLE p2p_agent_deployments ADD COLUMN IF NOT EXISTS lease_expires_at TIMESTAMPTZ`})
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: agent deployment public event cursor v96", Up: func(ctx context.Context, txn *sql.Tx) error {
		return execMigrationStatements(ctx, txn, []string{
			`ALTER TABLE p2p_agent_deployment_events ADD COLUMN IF NOT EXISTS public_sequence BIGINT`,
			`ALTER TABLE p2p_agent_deployment_events ADD COLUMN IF NOT EXISTS event_id TEXT`,
			`WITH ranked AS (SELECT owner_id,workload_id,operation_id,sequence,ROW_NUMBER() OVER (PARTITION BY owner_id,workload_id ORDER BY created_at,operation_id,sequence) AS n FROM p2p_agent_deployment_events) UPDATE p2p_agent_deployment_events e SET public_sequence=r.n FROM ranked r WHERE e.owner_id=r.owner_id AND e.workload_id=r.workload_id AND e.operation_id=r.operation_id AND e.sequence=r.sequence AND e.public_sequence IS NULL`,
			`CREATE UNIQUE INDEX IF NOT EXISTS p2p_agent_deployment_events_public_cursor_idx ON p2p_agent_deployment_events(owner_id,workload_id,public_sequence) WHERE public_sequence IS NOT NULL`,
			`UPDATE p2p_agent_deployments SET operation_json=operation_json-'failure_summary', object_json=CASE WHEN object_json ? 'error' THEN jsonb_set(jsonb_set(object_json,'{error,summary}','"Deployment failed"'::jsonb,true),'{current_operation}',COALESCE(object_json->'current_operation','{}'::jsonb)-'failure_summary',true) ELSE object_json END`,
			`UPDATE p2p_agent_deployment_events SET event_json=jsonb_set(event_json,'{message}','""'::jsonb,true) WHERE event_json ? 'message'`,
		})
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: agent secret envelopes v97", Up: func(ctx context.Context, txn *sql.Tx) error {
		return execMigrationStatements(ctx, txn, []string{
			`CREATE TABLE IF NOT EXISTS p2p_agent_secrets (
				secret_domain TEXT NOT NULL CHECK (secret_domain <> ''),
				owner_id TEXT NOT NULL CHECK (owner_id <> ''),
				entity_id TEXT NOT NULL CHECK (entity_id <> ''),
				secret_revision BIGINT NOT NULL CHECK (secret_revision > 0),
				purpose TEXT NOT NULL CHECK (purpose <> ''),
				reference TEXT NOT NULL CHECK (reference <> ''),
				binding_digest BYTEA NOT NULL CHECK (octet_length(binding_digest) = 32),
				envelope_version SMALLINT NOT NULL DEFAULT 1 CHECK (envelope_version = 1),
				aad_version SMALLINT NOT NULL DEFAULT 1 CHECK (aad_version = 1),
				key_id TEXT NOT NULL CHECK (key_id <> ''),
				nonce BYTEA NOT NULL CHECK (octet_length(nonce) = 12),
				ciphertext BYTEA NOT NULL CHECK (octet_length(ciphertext) >= 16),
				created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
				PRIMARY KEY (secret_domain, owner_id, entity_id, secret_revision, purpose, reference)
			)`,
			`CREATE INDEX IF NOT EXISTS p2p_agent_secrets_key_usage_idx
				ON p2p_agent_secrets(key_id, secret_domain, owner_id, entity_id, secret_revision)`,
			`CREATE TABLE IF NOT EXISTS p2p_agent_secret_key_usage (
				secret_domain TEXT NOT NULL,
				owner_id TEXT NOT NULL,
				entity_id TEXT NOT NULL,
				secret_revision BIGINT NOT NULL CHECK (secret_revision > 0),
				purpose TEXT NOT NULL,
				reference TEXT NOT NULL,
				key_id TEXT NOT NULL CHECK (key_id <> ''),
				envelope_digest BYTEA NOT NULL CHECK (octet_length(envelope_digest) = 32),
				updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
				PRIMARY KEY (secret_domain, owner_id, entity_id, secret_revision, purpose, reference)
			)`,
			`CREATE INDEX IF NOT EXISTS p2p_agent_secret_key_usage_key_idx
				ON p2p_agent_secret_key_usage(key_id, secret_domain)`,
			`CREATE TABLE IF NOT EXISTS p2p_agent_secret_rotations (
				rotation_id UUID PRIMARY KEY,
				state TEXT NOT NULL CHECK (state IN ('rewrapping','verifying','complete','failed')),
				from_key_ids TEXT[] NOT NULL,
				to_key_id TEXT NOT NULL CHECK (to_key_id <> ''),
				lease_owner TEXT NOT NULL DEFAULT '',
				lease_epoch BIGINT NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
				lease_expires_at TIMESTAMPTZ,
				cursor_domain TEXT NOT NULL DEFAULT '',
				cursor_owner_id TEXT NOT NULL DEFAULT '',
				cursor_entity_id TEXT NOT NULL DEFAULT '',
				cursor_revision BIGINT NOT NULL DEFAULT 0 CHECK (cursor_revision >= 0),
				rewrapped_rows BIGINT NOT NULL DEFAULT 0 CHECK (rewrapped_rows >= 0),
				verified_rows BIGINT NOT NULL DEFAULT 0 CHECK (verified_rows >= 0),
				error_code TEXT NOT NULL DEFAULT '',
				created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
				updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
				completed_at TIMESTAMPTZ
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS p2p_agent_secret_rotations_live_idx
				ON p2p_agent_secret_rotations((TRUE))
				WHERE state IN ('rewrapping','verifying')`,
			`ALTER TABLE p2p_agent_model_profiles ADD COLUMN IF NOT EXISTS api_key_key_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE p2p_agent_model_profiles ADD COLUMN IF NOT EXISTS api_key_profile_revision BIGINT NOT NULL DEFAULT 0`,
			`ALTER TABLE p2p_agent_model_profile_credentials ADD COLUMN IF NOT EXISTS api_key_key_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE p2p_agent_model_profile_credentials ADD COLUMN IF NOT EXISTS profile_revision BIGINT NOT NULL DEFAULT 0`,
		})
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: agent tasks and confirmations v98", Up: func(ctx context.Context, txn *sql.Tx) error {
		if err := execMigrationDDL(ctx, txn, AgentTaskDDL); err != nil {
			return err
		}
		return execMigrationDDL(ctx, txn, AgentConfirmationDDL)
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: agent extension lifecycle v99", Up: func(ctx context.Context, txn *sql.Tx) error {
		return execMigrationDDL(ctx, txn, AgentExtensionDDL)
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: AWS control plane v100", Up: func(ctx context.Context, txn *sql.Tx) error {
		return execMigrationDDL(ctx, txn, AgentAWSDDL)
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: workload control plane v101", Up: func(ctx context.Context, txn *sql.Tx) error {
		if err := execMigrationDDL(ctx, txn, AgentWorkloadDDL); err != nil {
			return err
		}
		return execMigrationStatements(ctx, txn, []string{
			`CREATE TABLE IF NOT EXISTS core_workload_event_counters (
				owner_id text NOT NULL, operation_id uuid NOT NULL,
				next_sequence bigint NOT NULL DEFAULT 1 CHECK (next_sequence > 0),
				PRIMARY KEY(owner_id, operation_id)
			)`,
		})
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: generic schedules and deployment cursors v102", Up: func(ctx context.Context, txn *sql.Tx) error {
		if err := execMigrationStatements(ctx, txn, []string{
			`ALTER TABLE p2p_agent_schedules ADD COLUMN IF NOT EXISTS task_template JSONB NOT NULL DEFAULT '{}'::jsonb`,
			`ALTER TABLE p2p_agent_schedules ADD COLUMN IF NOT EXISTS core_state TEXT NOT NULL DEFAULT 'active' CHECK (core_state IN ('active','paused'))`,
			`ALTER TABLE p2p_agent_schedules ADD COLUMN IF NOT EXISTS trigger_json JSONB NOT NULL DEFAULT '{}'::jsonb`,
			`UPDATE p2p_agent_schedules SET task_template=jsonb_strip_nulls(jsonb_build_object('goal',prompt,'model_profile_id',NULLIF(model_profile_id,''))) WHERE task_template='{}'::jsonb AND (prompt<>'' OR model_profile_id<>'')`,
			`UPDATE p2p_agent_schedules SET core_state=CASE WHEN status='disabled' THEN 'paused' ELSE 'active' END`,
			`UPDATE p2p_agent_schedules SET trigger_json=CASE LOWER(trigger_kind) WHEN 'run_at' THEN jsonb_build_object('kind','run_at','run_at',trigger_value) WHEN 'one_time' THEN jsonb_build_object('kind','run_at','run_at',trigger_value) WHEN 'cron' THEN jsonb_build_object('kind','cron','expression',trigger_value,'timezone',timezone) ELSE '{}'::jsonb END WHERE trigger_json='{}'::jsonb`,
			`ALTER TABLE p2p_agent_schedule_runs ADD COLUMN IF NOT EXISTS occurrence_id UUID`,
			`ALTER TABLE p2p_agent_schedule_runs ADD COLUMN IF NOT EXISTS task_id UUID`,
			`CREATE UNIQUE INDEX IF NOT EXISTS p2p_agent_schedule_runs_occurrence_link_idx ON p2p_agent_schedule_runs(owner_id,schedule_id,occurrence_id) WHERE occurrence_id IS NOT NULL`,
			`CREATE INDEX IF NOT EXISTS p2p_agent_schedule_runs_task_idx ON p2p_agent_schedule_runs(owner_id,task_id) WHERE task_id IS NOT NULL`,
			`CREATE INDEX IF NOT EXISTS agent_schedule_occurrences_owner_schedule_idx ON agent_schedule_occurrences(owner_id,schedule_id,scheduled_for)`,
			`CREATE INDEX IF NOT EXISTS agent_schedule_occurrences_owner_task_idx ON agent_schedule_occurrences(owner_id,task_id)`,
			`UPDATE p2p_agent_schedule_runs r SET occurrence_id=o.occurrence_id,task_id=o.task_id FROM agent_schedule_occurrences o WHERE r.occurrence_id IS NULL AND o.run_id::text=r.run_id AND o.owner_id=r.owner_id`,
			`CREATE TABLE IF NOT EXISTS p2p_agent_deployment_event_cursors (
				owner_id TEXT NOT NULL, workload_id TEXT NOT NULL,
				last_sequence BIGINT NOT NULL DEFAULT 0 CHECK (last_sequence >= 0),
				updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
				PRIMARY KEY(owner_id, workload_id)
			)`,
			`ALTER TABLE core_workload_events ADD COLUMN IF NOT EXISTS workload_id UUID`,
			`ALTER TABLE core_workload_events ADD COLUMN IF NOT EXISTS public_sequence BIGINT`,
			`UPDATE core_workload_events e
			 SET workload_id=o.workload_id
			 FROM core_workload_operations o
			 WHERE e.owner_id=o.owner_id AND e.operation_id=o.operation_id AND e.workload_id IS NULL`,
			`WITH ranked AS (
				SELECT e.owner_id,e.operation_id,e.sequence,
				       ROW_NUMBER() OVER (PARTITION BY e.owner_id,e.workload_id ORDER BY e.at,e.operation_id,e.sequence) AS public_sequence
				FROM core_workload_events e
			 )
			 UPDATE core_workload_events e
			 SET public_sequence=r.public_sequence
			 FROM ranked r
			 WHERE e.owner_id=r.owner_id AND e.operation_id=r.operation_id AND e.sequence=r.sequence
			   AND e.public_sequence IS NULL`,
			`ALTER TABLE core_workload_events ALTER COLUMN workload_id SET NOT NULL`,
			`ALTER TABLE core_workload_events ALTER COLUMN public_sequence SET NOT NULL`,
			`CREATE UNIQUE INDEX IF NOT EXISTS core_workload_events_public_sequence_idx
			 ON core_workload_events(owner_id,workload_id,public_sequence)`,
			`DO $$ BEGIN
			 IF NOT EXISTS (
				SELECT 1 FROM pg_constraint
				WHERE conname='core_workload_events_workload_fk'
				  AND conrelid=to_regclass('core_workload_events')
				  AND connamespace=(SELECT relnamespace FROM pg_class WHERE oid=to_regclass('core_workload_events'))
			 ) THEN
				ALTER TABLE core_workload_events
				ADD CONSTRAINT core_workload_events_workload_fk
				FOREIGN KEY(owner_id,workload_id) REFERENCES core_workloads(owner_id,workload_id) ON DELETE RESTRICT;
			 END IF;
			 END $$`,
			`INSERT INTO p2p_agent_deployment_event_cursors(owner_id,workload_id,last_sequence,updated_at)
			 SELECT owner_id,workload_id,COALESCE(MAX(public_sequence),0),COALESCE(MAX(created_at),NOW())
			 FROM p2p_agent_deployment_events GROUP BY owner_id,workload_id
			 ON CONFLICT(owner_id,workload_id) DO UPDATE SET last_sequence=GREATEST(p2p_agent_deployment_event_cursors.last_sequence,EXCLUDED.last_sequence),updated_at=GREATEST(p2p_agent_deployment_event_cursors.updated_at,EXCLUDED.updated_at)`,
			`INSERT INTO p2p_agent_deployment_event_cursors(owner_id,workload_id,last_sequence,updated_at)
			 SELECT owner_id,workload_id,COALESCE(MAX(public_sequence),0),COALESCE(MAX(at),NOW())
			 FROM core_workload_events GROUP BY owner_id,workload_id
			 ON CONFLICT(owner_id,workload_id) DO UPDATE SET last_sequence=GREATEST(p2p_agent_deployment_event_cursors.last_sequence,EXCLUDED.last_sequence),updated_at=GREATEST(p2p_agent_deployment_event_cursors.updated_at,EXCLUDED.updated_at)`,
		}); err != nil {
			return err
		}
		// A partially-applied deployment may already have created the named
		// foreign key. Check the catalog before adding it so v102 remains
		// restart-safe without registering a second migration version.
		var exists bool
		if err := txn.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1
			FROM pg_constraint c
			WHERE c.conname='p2p_agent_schedule_runs_task_fk'
			  AND c.conrelid=to_regclass('p2p_agent_schedule_runs')
			  AND c.connamespace=(SELECT relnamespace FROM pg_class WHERE oid=to_regclass('p2p_agent_schedule_runs'))
		)`).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if _, err := txn.ExecContext(ctx, `ALTER TABLE p2p_agent_schedule_runs ADD CONSTRAINT p2p_agent_schedule_runs_task_fk FOREIGN KEY (owner_id,task_id) REFERENCES agent_tasks(owner_id,task_id) ON DELETE SET NULL (task_id)`); err != nil {
				return err
			}
		}
		if err := txn.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1
			FROM pg_constraint c
			WHERE c.conname='p2p_agent_schedule_runs_schedule_fk'
			  AND c.conrelid=to_regclass('p2p_agent_schedule_runs')
			  AND c.connamespace=(SELECT relnamespace FROM pg_class WHERE oid=to_regclass('p2p_agent_schedule_runs'))
		)`).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if _, err := txn.ExecContext(ctx, `ALTER TABLE p2p_agent_schedule_runs ADD CONSTRAINT p2p_agent_schedule_runs_schedule_fk FOREIGN KEY (owner_id,schedule_id) REFERENCES p2p_agent_schedules(owner_id,schedule_id) ON DELETE RESTRICT`); err != nil {
				return err
			}
		}
		return nil
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: typed EC2 provision readback v103", Up: func(ctx context.Context, txn *sql.Tx) error {
		return execMigrationStatements(ctx, txn, []string{
			`CREATE TABLE IF NOT EXISTS core_aws_ec2_provisions (
				owner_id TEXT NOT NULL, provision_id UUID NOT NULL, plan_id UUID NOT NULL,
				credential_id UUID NOT NULL, credential_revision BIGINT NOT NULL CHECK (credential_revision > 0),
				region TEXT NOT NULL, stack_name TEXT NOT NULL, profile TEXT NOT NULL,
				owner_digest TEXT NOT NULL CHECK (owner_digest ~ '^sha256:[a-f0-9]{64}$'),
				plan_revision BIGINT NOT NULL CHECK (plan_revision > 0), template_sha256 TEXT NOT NULL CHECK (template_sha256 ~ '^[a-f0-9]{64}$'),
				plan_digest TEXT NOT NULL CHECK (plan_digest ~ '^[a-f0-9]{64}$'), state TEXT NOT NULL CHECK (state IN ('planned','creating','active','destroying','destroyed','uncertain','failed')),
				revision BIGINT NOT NULL CHECK (revision > 0), create_change_id UUID, destroy_change_id UUID, active_change_id UUID,
				stack_id TEXT NOT NULL DEFAULT '', instance_id TEXT NOT NULL DEFAULT '', public_ip TEXT NOT NULL DEFAULT '', security_group_id TEXT NOT NULL DEFAULT '',
				output_digest TEXT NOT NULL DEFAULT '', observed_at TIMESTAMPTZ, reconciliation_required BOOLEAN NOT NULL DEFAULT FALSE,
				error_code TEXT NOT NULL DEFAULT '', error_summary TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
				PRIMARY KEY(owner_id,provision_id), UNIQUE(owner_id,plan_id),
				FOREIGN KEY(owner_id,plan_id) REFERENCES core_aws_plans(owner_id,plan_id) ON DELETE RESTRICT,
				FOREIGN KEY(owner_id,credential_id,credential_revision) REFERENCES core_aws_credentials(owner_id,credential_id,revision) ON DELETE RESTRICT
			)`,
			`CREATE TABLE IF NOT EXISTS core_aws_ec2_provision_events (
				owner_id TEXT NOT NULL, provision_id UUID NOT NULL, change_id UUID, sequence BIGINT NOT NULL, event_id UUID NOT NULL, kind TEXT NOT NULL, revision BIGINT NOT NULL, at TIMESTAMPTZ NOT NULL,
				PRIMARY KEY(owner_id,provision_id,sequence), FOREIGN KEY(owner_id,provision_id) REFERENCES core_aws_ec2_provisions(owner_id,provision_id) ON DELETE CASCADE
			)`,
			`ALTER TABLE core_aws_ec2_provision_events ADD COLUMN IF NOT EXISTS change_id UUID`,
			`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='core_aws_ec2_provision_events_change_fk' AND conrelid='core_aws_ec2_provision_events'::regclass) THEN ALTER TABLE core_aws_ec2_provision_events ADD CONSTRAINT core_aws_ec2_provision_events_change_fk FOREIGN KEY(owner_id,change_id) REFERENCES core_aws_changes(owner_id,change_id) ON DELETE RESTRICT; END IF; END $$`,
			`CREATE TABLE IF NOT EXISTS core_aws_ec2_provision_event_counters (owner_id TEXT NOT NULL, provision_id UUID NOT NULL, next_sequence BIGINT NOT NULL CHECK(next_sequence>0), PRIMARY KEY(owner_id,provision_id), FOREIGN KEY(owner_id,provision_id) REFERENCES core_aws_ec2_provisions(owner_id,provision_id) ON DELETE CASCADE)`,
			`ALTER TABLE core_aws_changes ADD COLUMN IF NOT EXISTS provision_id UUID`,
			`CREATE UNIQUE INDEX IF NOT EXISTS core_aws_changes_active_provision_idx ON core_aws_changes(owner_id,provision_id) WHERE provision_id IS NOT NULL AND status IN ('waiting_user','running')`,
			`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='core_aws_changes_provision_fk' AND conrelid='core_aws_changes'::regclass) THEN ALTER TABLE core_aws_changes ADD CONSTRAINT core_aws_changes_provision_fk FOREIGN KEY(owner_id,provision_id) REFERENCES core_aws_ec2_provisions(owner_id,provision_id) ON DELETE RESTRICT; END IF; END $$`,
		})
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: durable EC2 provision mutation leases v104", Up: func(ctx context.Context, txn *sql.Tx) error {
		return execMigrationStatements(ctx, txn, []string{
			`CREATE TABLE IF NOT EXISTS core_aws_ec2_provision_mutation_leases (
				owner_id TEXT NOT NULL,
				provision_id UUID NOT NULL,
				token UUID,
				epoch BIGINT NOT NULL DEFAULT 0 CHECK (epoch >= 0),
				expires_at TIMESTAMPTZ,
				state TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active','uncertain')),
				operation_id UUID,
				updated_at TIMESTAMPTZ NOT NULL,
				PRIMARY KEY(owner_id,provision_id),
				FOREIGN KEY(owner_id,provision_id) REFERENCES core_aws_ec2_provisions(owner_id,provision_id) ON DELETE CASCADE,
				CHECK ((token IS NULL AND expires_at IS NULL) OR (token IS NOT NULL AND expires_at IS NOT NULL))
			)`,
			`CREATE INDEX IF NOT EXISTS core_aws_ec2_provision_mutation_leases_expiry_idx ON core_aws_ec2_provision_mutation_leases(owner_id,expires_at)`,
			`ALTER TABLE core_aws_ec2_provision_mutation_leases ADD COLUMN IF NOT EXISTS state TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active','uncertain'))`,
			`ALTER TABLE core_aws_ec2_provision_mutation_leases ADD COLUMN IF NOT EXISTS operation_id UUID`,
			`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='core_aws_ec2_provision_mutation_leases_operation_fk' AND conrelid='core_aws_ec2_provision_mutation_leases'::regclass) THEN ALTER TABLE core_aws_ec2_provision_mutation_leases ADD CONSTRAINT core_aws_ec2_provision_mutation_leases_operation_fk FOREIGN KEY(owner_id,operation_id) REFERENCES core_workload_operations(owner_id,operation_id) ON DELETE RESTRICT; END IF; END $$`,
			`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='core_aws_ec2_provision_mutation_leases_uncertain_op_check' AND conrelid='core_aws_ec2_provision_mutation_leases'::regclass) THEN ALTER TABLE core_aws_ec2_provision_mutation_leases ADD CONSTRAINT core_aws_ec2_provision_mutation_leases_uncertain_op_check CHECK (state <> 'uncertain' OR operation_id IS NOT NULL); END IF; END $$`,
			`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='core_aws_ec2_provision_mutation_leases_unbound_check' AND conrelid='core_aws_ec2_provision_mutation_leases'::regclass) THEN ALTER TABLE core_aws_ec2_provision_mutation_leases ADD CONSTRAINT core_aws_ec2_provision_mutation_leases_unbound_check CHECK (token IS NOT NULL OR (expires_at IS NULL AND operation_id IS NULL)); END IF; END $$`,
			`CREATE INDEX IF NOT EXISTS core_aws_ec2_provision_mutation_leases_operation_idx ON core_aws_ec2_provision_mutation_leases(owner_id,operation_id) WHERE operation_id IS NOT NULL`,
		})
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: workload expected revision fence v105", Up: func(ctx context.Context, txn *sql.Tx) error {
		return execMigrationStatements(ctx, txn, []string{
			`ALTER TABLE core_workload_operations ADD COLUMN IF NOT EXISTS expected_workload_revision BIGINT NOT NULL DEFAULT 1 CHECK (expected_workload_revision > 0)`,
			`UPDATE core_workload_operations o SET expected_workload_revision=w.revision FROM core_workloads w WHERE w.owner_id=o.owner_id AND w.workload_id=o.workload_id AND o.expected_workload_revision=1 AND w.revision<>1`,
		})
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: unified deployment ledger v106", Up: func(ctx context.Context, txn *sql.Tx) error {
		if err := execMigrationStatements(ctx, txn, []string{
			`CREATE TABLE IF NOT EXISTS core_deployments (
				owner_id TEXT NOT NULL,
				deployment_id UUID NOT NULL,
				provision_id UUID,
				workload_id UUID,
				state TEXT NOT NULL DEFAULT 'pending',
				target_kind TEXT NOT NULL DEFAULT '',
				revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
				object_json JSONB NOT NULL DEFAULT '{}'::jsonb,
				operation_json JSONB NOT NULL DEFAULT '{}'::jsonb,
				actual_json JSONB NOT NULL DEFAULT '{}'::jsonb,
				quote_json JSONB NOT NULL DEFAULT '{}'::jsonb,
				created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
				updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
				PRIMARY KEY(owner_id,deployment_id),
				FOREIGN KEY(owner_id,provision_id) REFERENCES core_aws_ec2_provisions(owner_id,provision_id) ON DELETE RESTRICT,
				FOREIGN KEY(owner_id,workload_id) REFERENCES core_workloads(owner_id,workload_id) ON DELETE RESTRICT
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS core_deployments_owner_provision_uidx ON core_deployments(owner_id,provision_id) WHERE provision_id IS NOT NULL`,
			`CREATE UNIQUE INDEX IF NOT EXISTS core_deployments_owner_workload_uidx ON core_deployments(owner_id,workload_id) WHERE workload_id IS NOT NULL`,
			`CREATE INDEX IF NOT EXISTS core_deployments_owner_updated_idx ON core_deployments(owner_id,updated_at DESC,deployment_id DESC)`,
			`CREATE TABLE IF NOT EXISTS core_deployment_event_counters (
				owner_id TEXT NOT NULL,
				deployment_id UUID NOT NULL,
				next_sequence BIGINT NOT NULL DEFAULT 1 CHECK (next_sequence > 0),
				PRIMARY KEY(owner_id,deployment_id),
				FOREIGN KEY(owner_id,deployment_id) REFERENCES core_deployments(owner_id,deployment_id) ON DELETE CASCADE
			)`,
			`CREATE TABLE IF NOT EXISTS core_deployment_events (
				owner_id TEXT NOT NULL,
				deployment_id UUID NOT NULL,
				event_id UUID NOT NULL,
				sequence BIGINT NOT NULL CHECK (sequence > 0),
				source_kind TEXT NOT NULL CHECK (source_kind IN ('provision','workload')),
				source_id UUID NOT NULL,
				source_sequence BIGINT NOT NULL CHECK (source_sequence > 0),
				event_json JSONB NOT NULL DEFAULT '{}'::jsonb,
				created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
				PRIMARY KEY(owner_id,deployment_id,sequence),
				UNIQUE(owner_id,deployment_id,source_kind,source_id,source_sequence),
				FOREIGN KEY(owner_id,deployment_id) REFERENCES core_deployments(owner_id,deployment_id) ON DELETE CASCADE
			)`,
			`CREATE INDEX IF NOT EXISTS core_deployment_events_owner_source_idx ON core_deployment_events(owner_id,source_kind,source_id,source_sequence)`,
		}); err != nil {
			return err
		}
		// Reconstruct only mappings whose immutable typed identities prove the
		// relation. Legacy Message Server ledger rows without a provision id are
		// intentionally not imported into this deployment namespace.
		if err := execMigrationStatements(ctx, txn, []string{
			`INSERT INTO core_deployments(owner_id,deployment_id,provision_id,state,target_kind,revision,object_json,created_at,updated_at)
			 SELECT p.owner_id,md5('dirextalk:deployment:v1:'||p.owner_id||':'||p.provision_id::text)::uuid,p.provision_id,p.state,'AWS_EC2',p.revision,
			 jsonb_build_object('deployment_id',md5('dirextalk:deployment:v1:'||p.owner_id||':'||p.provision_id::text)::uuid,'provision_id',p.provision_id,'plan_id',p.plan_id,'plan_digest',p.plan_digest,'target_kind','AWS_EC2','status',p.state,'revision',p.revision),p.created_at,p.updated_at
			 FROM core_aws_ec2_provisions p ON CONFLICT DO NOTHING`,
			`UPDATE core_deployments d SET workload_id=w.workload_id,state='pending',target_kind=w.target_kind,revision=d.revision+1,object_json=jsonb_set(jsonb_set(d.object_json,'{workload_id}',to_jsonb(w.workload_id::text),true),'{target_kind}',to_jsonb(w.target_kind::text),true),updated_at=GREATEST(d.updated_at,w.updated_at)
			 FROM core_workloads w JOIN core_workload_plans p ON p.owner_id=w.owner_id AND p.plan_id=w.plan_id
			 JOIN core_aws_ec2_provisions ap ON ap.owner_id=w.owner_id AND ap.provision_id::text=(p.plan_json->'target'->'labels'->>'dirextalk:provision-id')
			 JOIN p2p_agent_secrets sec ON sec.owner_id=ap.owner_id AND sec.entity_id=ap.credential_id::text AND sec.reference=ap.credential_id::text AND sec.secret_revision=ap.credential_revision AND sec.secret_domain='aws' AND sec.purpose='credential'
			 WHERE d.owner_id=w.owner_id AND d.provision_id=ap.provision_id AND d.workload_id IS NULL
			 AND p.plan_json->'target'->'required_instance_tags'->>'dirextalk:plan-id'=ap.plan_id::text
			 AND p.plan_json->'target'->'labels'->>'dirextalk:provision-revision'=ap.revision::text
			 AND p.plan_json->'target'->>'region'=ap.region
			 AND p.plan_json->'target'->'required_instance_tags'->>'owner'=ap.owner_digest
			 AND jsonb_array_length(COALESCE(p.plan_json->'secret_grant_refs','[]'::jsonb))=1
			 AND p.plan_json->'secret_grant_refs'->0->>'reference_id'=ap.credential_id::text
			 AND p.plan_json->'secret_grant_refs'->0->>'purpose'='aws_credential'
			 AND (p.plan_json->'secret_grant_refs'->0->>'secret_revision')::bigint=ap.credential_revision
			 AND p.plan_json->'secret_grant_refs'->0->>'binding_digest'=encode(sec.binding_digest,'hex')`,
			`WITH source_events AS (
			 SELECT d.owner_id,d.deployment_id,e.event_id,e.provision_id AS source_id,e.change_id::text,e.sequence AS source_sequence,'provision'::text AS source_kind,e.kind,''::text AS status,''::text AS operation,''::text AS message,NULL::jsonb AS readback_json,e.at
			 FROM core_aws_ec2_provision_events e JOIN core_deployments d ON d.owner_id=e.owner_id AND d.provision_id=e.provision_id
			 UNION ALL
			 SELECT d.owner_id,d.deployment_id,md5(d.owner_id||':workload:'||e.operation_id::text||':'||e.sequence::text)::uuid,e.operation_id,NULL::text,e.sequence,'workload'::text,e.kind,e.status,o.operation,COALESCE(e.message,''),e.readback_json,e.at
			 FROM core_workload_events e JOIN core_deployments d ON d.owner_id=e.owner_id AND d.workload_id=e.workload_id JOIN core_workload_operations o ON o.owner_id=e.owner_id AND o.operation_id=e.operation_id
			 UNION ALL
			 SELECT d.owner_id,d.deployment_id,md5(d.owner_id||':legacy:'||e.operation_id||':'||e.sequence::text)::uuid,md5(d.owner_id||':legacy:'||e.operation_id)::uuid,NULL::text,e.sequence,'workload'::text,COALESCE(e.event_json->>'type','legacy'),COALESCE(e.event_json->>'status',''),COALESCE(e.event_json->>'operation',''),'',NULL::jsonb,e.created_at
			 FROM p2p_agent_deployment_events e JOIN core_deployments d ON d.owner_id=e.owner_id AND d.workload_id::text=e.workload_id
			 WHERE e.operation_id<>'' AND NOT EXISTS (SELECT 1 FROM core_workload_events ce WHERE ce.owner_id=d.owner_id AND ce.workload_id=d.workload_id)
			), ranked AS (
			 SELECT s.*,row_number() OVER (PARTITION BY owner_id,deployment_id ORDER BY at,source_kind,source_id,source_sequence) AS public_sequence
			 FROM source_events s
			)
			INSERT INTO core_deployment_events(owner_id,deployment_id,event_id,sequence,source_kind,source_id,source_sequence,event_json,created_at)
			 SELECT owner_id,deployment_id,event_id,public_sequence,source_kind,source_id,source_sequence,jsonb_build_object('kind',kind,'status',status,'operation',operation,'message',message,'change_id',COALESCE(change_id,''),'actual',jsonb_build_object('state',readback_json->>'state','applied_plan_id',readback_json->>'applied_plan_id','applied_plan_digest',readback_json->>'applied_plan_digest','readback_digest',readback_json->>'readback_digest'),'at',at),at FROM ranked
			 ON CONFLICT DO NOTHING`,
			`INSERT INTO core_deployment_event_counters(owner_id,deployment_id,next_sequence)
			 SELECT owner_id,deployment_id,COUNT(*)+1 FROM core_deployment_events GROUP BY owner_id,deployment_id
			 ON CONFLICT(owner_id,deployment_id) DO UPDATE SET next_sequence=GREATEST(core_deployment_event_counters.next_sequence,EXCLUDED.next_sequence)`,
			`WITH latest AS (SELECT DISTINCT ON (owner_id,deployment_id) owner_id,deployment_id,event_json->>'status' AS status,event_json->>'operation' AS operation,created_at FROM core_deployment_events WHERE event_json->>'status' IN ('succeeded','completed','failed','uncertain','destroyed') ORDER BY owner_id,deployment_id,sequence DESC)
			 UPDATE core_deployments d SET state=CASE WHEN l.status IN ('succeeded','completed') AND l.operation='destroy' THEN 'destroyed' WHEN l.status IN ('succeeded','completed') THEN 'ready' WHEN l.status='failed' THEN 'failed' WHEN l.status='uncertain' THEN 'uncertain' WHEN l.status='destroyed' THEN 'destroyed' ELSE d.state END,revision=d.revision+1,object_json=jsonb_set(d.object_json,'{status}',to_jsonb(CASE WHEN l.status IN ('succeeded','completed') AND l.operation='destroy' THEN 'destroyed' WHEN l.status IN ('succeeded','completed') THEN 'succeeded' WHEN l.status='failed' THEN 'failed' WHEN l.status='uncertain' THEN 'uncertain' WHEN l.status='destroyed' THEN 'destroyed' ELSE d.object_json->>'status' END),true),updated_at=GREATEST(d.updated_at,l.created_at)
			 FROM latest l WHERE d.owner_id=l.owner_id AND d.deployment_id=l.deployment_id AND l.status IN ('succeeded','completed','failed','uncertain','destroyed')`,
		}); err != nil {
			return err
		}
		// Source events are fanned out in their original transaction. The
		// deployment counter row is the serialization point; the source tuple
		// unique key makes retries/replayed provider responses idempotent.
		if err := execMigrationStatements(ctx, txn, []string{
			`CREATE OR REPLACE FUNCTION core_fanout_workload_event() RETURNS trigger LANGUAGE plpgsql AS $$
			DECLARE d UUID; seq BIGINT; payload JSONB; op_kind TEXT; public_state TEXT;
			BEGIN
				SELECT deployment_id INTO d FROM core_deployments WHERE owner_id=NEW.owner_id AND workload_id=NEW.workload_id FOR UPDATE;
				IF d IS NULL THEN RETURN NEW; END IF;
				IF EXISTS (SELECT 1 FROM core_deployment_events WHERE owner_id=NEW.owner_id AND deployment_id=d AND source_kind='workload' AND source_id=NEW.operation_id AND source_sequence=NEW.sequence) THEN RETURN NEW; END IF;
				INSERT INTO core_deployment_event_counters(owner_id,deployment_id,next_sequence) VALUES(NEW.owner_id,d,2)
				ON CONFLICT(owner_id,deployment_id) DO UPDATE SET next_sequence=core_deployment_event_counters.next_sequence+1
				RETURNING next_sequence-1 INTO seq;
				payload := jsonb_build_object('kind',NEW.kind,'status',NEW.status,'message',COALESCE(NEW.message,''),'actual',jsonb_build_object('state',NEW.readback_json->>'state','applied_plan_id',NEW.readback_json->>'applied_plan_id','applied_plan_digest',NEW.readback_json->>'applied_plan_digest','readback_digest',NEW.readback_json->>'readback_digest','provider_version',NEW.readback_json->>'provider_version','observed_at',NEW.readback_json->>'observed_at'),'at',NEW.at);
				INSERT INTO core_deployment_events(owner_id,deployment_id,event_id,sequence,source_kind,source_id,source_sequence,event_json,created_at)
				VALUES(NEW.owner_id,d,md5(NEW.owner_id || ':workload:' || NEW.operation_id::text || ':' || NEW.sequence::text)::uuid,seq,'workload',NEW.operation_id,NEW.sequence,payload,NEW.at)
				ON CONFLICT(owner_id,deployment_id,source_kind,source_id,source_sequence) DO NOTHING;
				SELECT operation INTO op_kind FROM core_workload_operations WHERE owner_id=NEW.owner_id AND operation_id=NEW.operation_id;
				public_state := CASE WHEN NEW.status IN ('succeeded','completed') AND op_kind='destroy' THEN 'destroyed' WHEN NEW.status IN ('succeeded','completed') THEN 'ready' WHEN NEW.status IN ('failed','uncertain','canceled','expired','rejected') THEN NEW.status WHEN NEW.status IN ('running','dispatched') THEN 'running' ELSE NULL END;
				UPDATE core_deployments SET state=COALESCE(public_state,state),revision=revision+1,object_json=CASE WHEN public_state IS NULL THEN object_json ELSE jsonb_set(object_json,'{status}',to_jsonb(CASE WHEN public_state='ready' THEN 'succeeded' ELSE public_state END),true) END,actual_json=CASE WHEN NEW.readback_json IS NULL OR NEW.readback_json='null'::jsonb THEN actual_json ELSE NEW.readback_json END,updated_at=NEW.at WHERE owner_id=NEW.owner_id AND deployment_id=d;
				RETURN NEW;
			END $$`,
			`CREATE OR REPLACE FUNCTION core_fanout_provision_event() RETURNS trigger LANGUAGE plpgsql AS $$
			DECLARE d UUID; seq BIGINT; payload JSONB; current_state TEXT;
			BEGIN
				SELECT deployment_id INTO d FROM core_deployments WHERE owner_id=NEW.owner_id AND provision_id=NEW.provision_id FOR UPDATE;
				IF d IS NULL THEN RETURN NEW; END IF;
				IF EXISTS (SELECT 1 FROM core_deployment_events WHERE owner_id=NEW.owner_id AND deployment_id=d AND source_kind='provision' AND source_id=NEW.provision_id AND source_sequence=NEW.sequence) THEN RETURN NEW; END IF;
				INSERT INTO core_deployment_event_counters(owner_id,deployment_id,next_sequence) VALUES(NEW.owner_id,d,2)
				ON CONFLICT(owner_id,deployment_id) DO UPDATE SET next_sequence=core_deployment_event_counters.next_sequence+1
				RETURNING next_sequence-1 INTO seq;
				SELECT state INTO current_state FROM core_aws_ec2_provisions WHERE owner_id=NEW.owner_id AND provision_id=NEW.provision_id;
				payload := jsonb_build_object('kind',NEW.kind,'status',COALESCE(current_state,''),'change_id',COALESCE(NEW.change_id::text,''),'at',NEW.at);
				INSERT INTO core_deployment_events(owner_id,deployment_id,event_id,sequence,source_kind,source_id,source_sequence,event_json,created_at)
				VALUES(NEW.owner_id,d,md5(NEW.owner_id || ':provision:' || NEW.provision_id::text || ':' || NEW.sequence::text)::uuid,seq,'provision',NEW.provision_id,NEW.sequence,payload,NEW.at)
				ON CONFLICT(owner_id,deployment_id,source_kind,source_id,source_sequence) DO NOTHING;
				UPDATE core_deployments SET state=COALESCE(NULLIF(current_state,''),state),revision=revision+1,object_json=jsonb_set(object_json,'{status}',to_jsonb(COALESCE(NULLIF(current_state,''),state)),true),updated_at=NEW.at WHERE owner_id=NEW.owner_id AND deployment_id=d;
				RETURN NEW;
			END $$`,
			`DROP TRIGGER IF EXISTS core_workload_event_deployment_fanout ON core_workload_events`,
			`CREATE TRIGGER core_workload_event_deployment_fanout AFTER INSERT ON core_workload_events FOR EACH ROW EXECUTE FUNCTION core_fanout_workload_event()`,
			`DROP TRIGGER IF EXISTS core_provision_event_deployment_fanout ON core_aws_ec2_provision_events`,
			`CREATE TRIGGER core_provision_event_deployment_fanout AFTER INSERT ON core_aws_ec2_provision_events FOR EACH ROW EXECUTE FUNCTION core_fanout_provision_event()`,
		}); err != nil {
			return err
		}
		return nil
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: model credential envelope versions v107", Up: func(ctx context.Context, txn *sql.Tx) error {
		return execMigrationStatements(ctx, txn, []string{
			`ALTER TABLE p2p_agent_model_profiles ADD COLUMN IF NOT EXISTS api_key_envelope_version BIGINT NOT NULL DEFAULT 0`,
			`ALTER TABLE p2p_agent_model_profiles ADD COLUMN IF NOT EXISTS api_key_aad_version BIGINT NOT NULL DEFAULT 0`,
			`ALTER TABLE p2p_agent_model_profile_credentials ADD COLUMN IF NOT EXISTS api_key_envelope_version BIGINT NOT NULL DEFAULT 0`,
			`ALTER TABLE p2p_agent_model_profile_credentials ADD COLUMN IF NOT EXISTS api_key_aad_version BIGINT NOT NULL DEFAULT 0`,
			`DO $$ BEGIN
				IF EXISTS (SELECT 1 FROM p2p_agent_model_profiles WHERE
					(octet_length(api_key_ciphertext)=0 AND NOT (credential_version=0 AND api_key_key_id='' AND octet_length(api_key_nonce)=0 AND api_key_envelope_version=0 AND api_key_aad_version=0)) OR
					(octet_length(api_key_ciphertext)>0 AND NOT (((api_key_key_id='' AND api_key_envelope_version=0 AND api_key_aad_version=0) OR (api_key_key_id<>'' AND ((api_key_envelope_version=0 AND api_key_aad_version=0) OR (api_key_envelope_version=1 AND api_key_aad_version=1)))) AND credential_version>0 AND octet_length(api_key_nonce)=12 AND octet_length(api_key_ciphertext)>16))
				) THEN RAISE EXCEPTION 'invalid pre-v107 model credential envelope'; END IF;
				IF EXISTS (SELECT 1 FROM p2p_agent_model_profile_credentials WHERE
					credential_version<=0 OR octet_length(api_key_ciphertext)<=16 OR octet_length(api_key_nonce)<>12 OR
					NOT ((api_key_key_id='' AND api_key_envelope_version=0 AND api_key_aad_version=0) OR (api_key_key_id<>'' AND ((api_key_envelope_version=0 AND api_key_aad_version=0) OR (api_key_envelope_version=1 AND api_key_aad_version=1))))
				) THEN RAISE EXCEPTION 'invalid pre-v107 model credential history envelope'; END IF;
			END $$`,
			`UPDATE p2p_agent_model_profiles SET api_key_envelope_version=1,api_key_aad_version=1 WHERE octet_length(api_key_ciphertext)>0 AND api_key_key_id<>'' AND api_key_envelope_version=0 AND api_key_aad_version=0`,
			`UPDATE p2p_agent_model_profile_credentials SET api_key_envelope_version=1,api_key_aad_version=1 WHERE octet_length(api_key_ciphertext)>0 AND api_key_key_id<>'' AND api_key_envelope_version=0 AND api_key_aad_version=0`,
			`ALTER TABLE p2p_agent_model_profiles DROP CONSTRAINT IF EXISTS p2p_agent_model_profiles_api_key_envelope_check`,
			`ALTER TABLE p2p_agent_model_profiles ADD CONSTRAINT p2p_agent_model_profiles_api_key_envelope_check CHECK ((credential_version=0 AND octet_length(api_key_ciphertext)=0 AND api_key_key_id='' AND octet_length(api_key_nonce)=0 AND api_key_envelope_version=0 AND api_key_aad_version=0) OR (credential_version>0 AND octet_length(api_key_ciphertext)>16 AND octet_length(api_key_nonce)=12 AND ((api_key_key_id='' AND api_key_envelope_version=0 AND api_key_aad_version=0) OR (api_key_key_id<>'' AND api_key_envelope_version=1 AND api_key_aad_version=1))))`,
			`ALTER TABLE p2p_agent_model_profile_credentials DROP CONSTRAINT IF EXISTS p2p_agent_model_profile_credentials_api_key_envelope_check`,
			`ALTER TABLE p2p_agent_model_profile_credentials ADD CONSTRAINT p2p_agent_model_profile_credentials_api_key_envelope_check CHECK (credential_version>0 AND octet_length(api_key_ciphertext)>16 AND octet_length(api_key_nonce)=12 AND ((api_key_key_id='' AND api_key_envelope_version=0 AND api_key_aad_version=0) OR (api_key_key_id<>'' AND api_key_envelope_version=1 AND api_key_aad_version=1)))`,
		})
	}})
	m.AddMigrations(sqlutil.Migration{Version: "p2p: public deployment UUIDs v108", Up: func(ctx context.Context, txn *sql.Tx) error {
		return execMigrationStatements(ctx, txn, []string{
			// The legacy IDs are durable internal keys. Public IDs retain all
			// entropy but normalize the RFC UUID version and variant bits.
			`CREATE OR REPLACE FUNCTION core_canonical_public_uuid(value UUID) RETURNS UUID LANGUAGE SQL IMMUTABLE STRICT AS $$
				SELECT encode(set_byte(set_byte(uuid_send(value),6,(get_byte(uuid_send(value),6) & 15) | 48),8,(get_byte(uuid_send(value),8) & 63) | 128),'hex')::uuid
			$$`,
			`ALTER TABLE core_deployments ADD COLUMN IF NOT EXISTS public_deployment_id UUID`,
			`ALTER TABLE core_deployment_events ADD COLUMN IF NOT EXISTS public_event_id UUID`,
			`CREATE OR REPLACE FUNCTION core_fill_public_deployment_id() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF NEW.public_deployment_id IS NULL THEN NEW.public_deployment_id:=core_canonical_public_uuid(NEW.deployment_id); END IF;
				RETURN NEW;
			END $$`,
			`CREATE OR REPLACE FUNCTION core_fill_public_deployment_event_id() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF NEW.public_event_id IS NULL THEN NEW.public_event_id:=core_canonical_public_uuid(NEW.event_id); END IF;
				RETURN NEW;
			END $$`,
			`DROP TRIGGER IF EXISTS core_deployments_public_id_fill ON core_deployments`,
			`CREATE TRIGGER core_deployments_public_id_fill BEFORE INSERT OR UPDATE OF deployment_id,public_deployment_id ON core_deployments FOR EACH ROW EXECUTE FUNCTION core_fill_public_deployment_id()`,
			`DROP TRIGGER IF EXISTS core_deployment_events_public_id_fill ON core_deployment_events`,
			`CREATE TRIGGER core_deployment_events_public_id_fill BEFORE INSERT OR UPDATE OF event_id,public_event_id ON core_deployment_events FOR EACH ROW EXECUTE FUNCTION core_fill_public_deployment_event_id()`,
			`UPDATE core_deployments SET public_deployment_id=core_canonical_public_uuid(deployment_id) WHERE public_deployment_id IS NULL`,
			`UPDATE core_deployment_events SET public_event_id=core_canonical_public_uuid(event_id) WHERE public_event_id IS NULL`,
			`UPDATE core_deployments SET object_json=jsonb_set(COALESCE(object_json,'{}'::jsonb),'{deployment_id}',to_jsonb(public_deployment_id::text),true)`,
			`ALTER TABLE core_deployments ALTER COLUMN public_deployment_id SET NOT NULL`,
			`ALTER TABLE core_deployment_events ALTER COLUMN public_event_id SET NOT NULL`,
			`CREATE UNIQUE INDEX IF NOT EXISTS core_deployments_owner_public_deployment_uidx ON core_deployments(owner_id,public_deployment_id)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS core_deployment_events_owner_public_event_uidx ON core_deployment_events(owner_id,public_event_id)`,
			`CREATE OR REPLACE FUNCTION core_fanout_workload_event() RETURNS trigger LANGUAGE plpgsql AS $$
			DECLARE d UUID; seq BIGINT; payload JSONB; op_kind TEXT; public_state TEXT;
			BEGIN
				SELECT deployment_id INTO d FROM core_deployments WHERE owner_id=NEW.owner_id AND workload_id=NEW.workload_id FOR UPDATE;
				IF d IS NULL THEN RETURN NEW; END IF;
				IF EXISTS (SELECT 1 FROM core_deployment_events WHERE owner_id=NEW.owner_id AND deployment_id=d AND source_kind='workload' AND source_id=NEW.operation_id AND source_sequence=NEW.sequence) THEN RETURN NEW; END IF;
				INSERT INTO core_deployment_event_counters(owner_id,deployment_id,next_sequence) VALUES(NEW.owner_id,d,2)
				ON CONFLICT(owner_id,deployment_id) DO UPDATE SET next_sequence=core_deployment_event_counters.next_sequence+1 RETURNING next_sequence-1 INTO seq;
				payload := jsonb_build_object('kind',NEW.kind,'status',NEW.status,'message',COALESCE(NEW.message,''),'actual',jsonb_build_object('state',NEW.readback_json->>'state','applied_plan_id',NEW.readback_json->>'applied_plan_id','applied_plan_digest',NEW.readback_json->>'applied_plan_digest','readback_digest',NEW.readback_json->>'readback_digest','provider_version',NEW.readback_json->>'provider_version','observed_at',NEW.readback_json->>'observed_at'),'at',NEW.at);
				INSERT INTO core_deployment_events(owner_id,deployment_id,event_id,public_event_id,sequence,source_kind,source_id,source_sequence,event_json,created_at)
				VALUES(NEW.owner_id,d,md5(NEW.owner_id || ':workload:' || NEW.operation_id::text || ':' || NEW.sequence::text)::uuid,core_canonical_public_uuid(md5(NEW.owner_id || ':workload:' || NEW.operation_id::text || ':' || NEW.sequence::text)::uuid),seq,'workload',NEW.operation_id,NEW.sequence,payload,NEW.at)
				ON CONFLICT(owner_id,deployment_id,source_kind,source_id,source_sequence) DO NOTHING;
				SELECT operation INTO op_kind FROM core_workload_operations WHERE owner_id=NEW.owner_id AND operation_id=NEW.operation_id;
				public_state := CASE WHEN NEW.status IN ('succeeded','completed') AND op_kind='destroy' THEN 'destroyed' WHEN NEW.status IN ('succeeded','completed') THEN 'ready' WHEN NEW.status IN ('failed','uncertain','canceled','expired','rejected') THEN NEW.status WHEN NEW.status IN ('running','dispatched') THEN 'running' ELSE NULL END;
				UPDATE core_deployments SET state=COALESCE(public_state,state),revision=revision+1,object_json=CASE WHEN public_state IS NULL THEN object_json ELSE jsonb_set(object_json,'{status}',to_jsonb(CASE WHEN public_state='ready' THEN 'succeeded' ELSE public_state END),true) END,actual_json=CASE WHEN NEW.readback_json IS NULL OR NEW.readback_json='null'::jsonb THEN actual_json ELSE NEW.readback_json END,updated_at=NEW.at WHERE owner_id=NEW.owner_id AND deployment_id=d;
				RETURN NEW;
			END $$`,
			`CREATE OR REPLACE FUNCTION core_fanout_provision_event() RETURNS trigger LANGUAGE plpgsql AS $$
			DECLARE d UUID; seq BIGINT; payload JSONB; current_state TEXT;
			BEGIN
				SELECT deployment_id INTO d FROM core_deployments WHERE owner_id=NEW.owner_id AND provision_id=NEW.provision_id FOR UPDATE;
				IF d IS NULL THEN RETURN NEW; END IF;
				IF EXISTS (SELECT 1 FROM core_deployment_events WHERE owner_id=NEW.owner_id AND deployment_id=d AND source_kind='provision' AND source_id=NEW.provision_id AND source_sequence=NEW.sequence) THEN RETURN NEW; END IF;
				INSERT INTO core_deployment_event_counters(owner_id,deployment_id,next_sequence) VALUES(NEW.owner_id,d,2)
				ON CONFLICT(owner_id,deployment_id) DO UPDATE SET next_sequence=core_deployment_event_counters.next_sequence+1 RETURNING next_sequence-1 INTO seq;
				SELECT state INTO current_state FROM core_aws_ec2_provisions WHERE owner_id=NEW.owner_id AND provision_id=NEW.provision_id;
				payload := jsonb_build_object('kind',NEW.kind,'status',COALESCE(current_state,''),'change_id',COALESCE(NEW.change_id::text,''),'at',NEW.at);
				INSERT INTO core_deployment_events(owner_id,deployment_id,event_id,public_event_id,sequence,source_kind,source_id,source_sequence,event_json,created_at)
				VALUES(NEW.owner_id,d,md5(NEW.owner_id || ':provision:' || NEW.provision_id::text || ':' || NEW.sequence::text)::uuid,core_canonical_public_uuid(md5(NEW.owner_id || ':provision:' || NEW.provision_id::text || ':' || NEW.sequence::text)::uuid),seq,'provision',NEW.provision_id,NEW.sequence,payload,NEW.at)
				ON CONFLICT(owner_id,deployment_id,source_kind,source_id,source_sequence) DO NOTHING;
				UPDATE core_deployments SET state=COALESCE(NULLIF(current_state,''),state),revision=revision+1,object_json=jsonb_set(object_json,'{status}',to_jsonb(COALESCE(NULLIF(current_state,''),state)),true),updated_at=NEW.at WHERE owner_id=NEW.owner_id AND deployment_id=d;
				RETURN NEW;
			END $$`,
		})
	}})
	// v109 repairs only reservations whose linked AWS/workload execution is
	// durably terminal.  Older completion code retained an inactive JSON
	// envelope; because the live-target index keys any non-NULL consumed
	// reservation, those rows could block a subsequent retry.  Pending,
	// confirmed, running, and uncertain/reconciling executions are deliberately
	// excluded so their uniqueness fence remains intact.
	m.AddMigrations(sqlutil.Migration{Version: "p2p: release terminal confirmation reservations v109", Up: func(ctx context.Context, txn *sql.Tx) error {
		return execMigrationStatements(ctx, txn, []string{
			`UPDATE agent_confirmations c
			 SET reservation_json=NULL,revision=c.revision+1,updated_at=clock_timestamp()
			 FROM core_aws_changes ch
			 JOIN agent_tasks t ON t.owner_id=ch.owner_id AND t.task_id=ch.task_id
			 WHERE c.owner_id=ch.owner_id AND c.confirmation_id=ch.confirmation_id AND c.task_id=ch.task_id
			   AND c.state='consumed' AND c.reservation_json IS NOT NULL
			   AND c.reservation_json ? 'active'
			   AND jsonb_typeof(c.reservation_json->'active')='boolean'
			   AND (c.reservation_json->>'active')::boolean=false
			   AND ch.status IN ('succeeded','failed','canceled')
			   AND ch.stage IN ('succeeded','failed','canceled')
			   AND t.status IN ('succeeded','failed','canceled')
			   AND NOT EXISTS (
				 SELECT 1 FROM core_aws_changes live
				 WHERE live.owner_id=ch.owner_id AND live.confirmation_id=ch.confirmation_id
				   AND live.status IN ('waiting_user','running')
			   )`,
			`UPDATE agent_confirmations c
			 SET reservation_json=NULL,revision=c.revision+1,updated_at=clock_timestamp()
			 FROM core_workload_operations o
			 JOIN agent_tasks t ON t.owner_id=o.owner_id AND t.task_id=o.task_id
			 WHERE c.owner_id=o.owner_id AND c.confirmation_id=o.confirmation_id AND c.task_id=o.task_id
			   AND c.state='consumed' AND c.reservation_json IS NOT NULL
			   AND c.reservation_json ? 'active'
			   AND jsonb_typeof(c.reservation_json->'active')='boolean'
			   AND (c.reservation_json->>'active')::boolean=false
			   AND o.status IN ('succeeded','failed') AND o.dispatch_state='terminal'
			   AND t.status IN ('succeeded','failed','canceled')
			   AND NOT EXISTS (
				 SELECT 1 FROM core_workload_operations live
				 WHERE live.owner_id=o.owner_id AND live.confirmation_id=o.confirmation_id
				   AND live.status IN ('waiting_user','running')
			   )`,
		})
	}})
	return m.Up(ctx)
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
