package storage

import (
	"context"
	"database/sql"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
)

func (s *DatabaseStore) migrate(ctx context.Context) error {
	m := sqlutil.NewMigrator(s.db)
	// Message-server is a fresh ProductCore database after the hard split.
	// Keep the existing ordered builders as one deterministic schema assembly,
	// but record only this baseline and reject any prior P2P migration ledger.
	m.SetBaselineVersion("p2p: fresh ProductCore baseline v1")
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
		Version: "p2p: channel post visibility v78",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			// Channel visibility is ProductCore-owned. Agent/model/knowledge/
			// task/execution schema belongs exclusively to dirextalk-agent.
			exists, err := productTableExists(ctx, txn, "p2p_channel_posts")
			if err != nil || !exists {
				return err
			}
			return execMigrationStatements(ctx, txn, []string{
				`ALTER TABLE p2p_channel_posts ADD COLUMN IF NOT EXISTS visibility TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('public', 'private'))`,
				`CREATE INDEX IF NOT EXISTS p2p_channel_posts_channel_visibility_idx ON p2p_channel_posts(channel_id, visibility, origin_server_ts DESC, post_id DESC)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: product capability operations ledger v80",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`CREATE TABLE IF NOT EXISTS p2p_capability_operations (
					operation_id TEXT PRIMARY KEY NOT NULL CHECK (operation_id <> ''),
					capability_id TEXT NOT NULL CHECK (capability_id <> ''),
					operation TEXT NOT NULL CHECK (operation <> ''),
					owner_id TEXT NOT NULL CHECK (owner_id <> ''),
					account_generation BIGINT NOT NULL CHECK (account_generation > 0),
					root_request_digest BYTEA NOT NULL CHECK (octet_length(root_request_digest) = 32),
					request_digest BYTEA NOT NULL CHECK (octet_length(request_digest) = 32),
					state TEXT NOT NULL CHECK (state IN ('pending','running','completed','failed','cancelled','uncertain')),
					result_json JSONB,
					error_code TEXT NOT NULL DEFAULT '',
					error_message TEXT NOT NULL DEFAULT '',
					revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
					updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
					created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_capability_operations_owner_updated_idx ON p2p_capability_operations(owner_id, updated_at DESC, operation_id)`,
				`CREATE TABLE IF NOT EXISTS p2p_capability_operation_events (
					operation_id TEXT NOT NULL REFERENCES p2p_capability_operations(operation_id) ON DELETE CASCADE,
					sequence BIGINT NOT NULL CHECK (sequence > 0),
					event_json JSONB NOT NULL,
					created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
					PRIMARY KEY (operation_id, sequence)
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_capability_operation_events_created_idx ON p2p_capability_operation_events(operation_id, sequence)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: prepared Matrix capability events v81",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			return execMigrationStatements(ctx, txn, []string{
				`CREATE TABLE IF NOT EXISTS p2p_capability_matrix_prepared_events (
					operation_id TEXT PRIMARY KEY NOT NULL REFERENCES p2p_capability_operations(operation_id) ON DELETE CASCADE,
					capability_id TEXT NOT NULL CHECK (capability_id <> ''),
					operation TEXT NOT NULL CHECK (operation <> ''),
					owner_id TEXT NOT NULL CHECK (owner_id <> ''),
					account_generation BIGINT NOT NULL CHECK (account_generation > 0),
					root_request_digest BYTEA NOT NULL CHECK (octet_length(root_request_digest) = 32),
					logical_id TEXT NOT NULL DEFAULT '',
					post_id TEXT NOT NULL DEFAULT '',
					state TEXT NOT NULL DEFAULT 'prepared' CHECK (state IN ('prepared','sent','uncertain')),
					attempt BIGINT NOT NULL DEFAULT 1 CHECK (attempt > 0),
					revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
					receipt_json JSONB NOT NULL DEFAULT '{}'::jsonb,
					event_id TEXT NOT NULL CHECK (event_id <> ''),
					room_id TEXT NOT NULL CHECK (room_id <> ''),
					sender_mxid TEXT NOT NULL CHECK (sender_mxid <> ''),
					event_type TEXT NOT NULL CHECK (event_type <> ''),
					message_type TEXT NOT NULL DEFAULT '',
					origin_server_ts BIGINT NOT NULL,
					room_version TEXT NOT NULL CHECK (room_version <> ''),
					event_pdu BYTEA NOT NULL CHECK (octet_length(event_pdu) <= 65536),
					pdu_sha256 BYTEA NOT NULL CHECK (octet_length(pdu_sha256) = 32),
					content_digest BYTEA NOT NULL CHECK (octet_length(content_digest) = 32),
					created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
					updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
					UNIQUE (room_id, event_id)
				)`,
				`CREATE INDEX IF NOT EXISTS p2p_capability_matrix_prepared_events_owner_idx ON p2p_capability_matrix_prepared_events(owner_id, account_generation, updated_at DESC)`,
			})
		},
	})
	m.AddMigrations(sqlutil.Migration{
		Version: "p2p: channel post settings v80",
		Up: func(ctx context.Context, txn *sql.Tx) error {
			exists, err := productTableExists(ctx, txn, "p2p_channel_posts")
			if err != nil || !exists {
				return err
			}
			return execMigrationStatements(ctx, txn, []string{
				`ALTER TABLE p2p_channel_posts ADD COLUMN IF NOT EXISTS comments_enabled BOOLEAN NOT NULL DEFAULT TRUE`,
				`ALTER TABLE p2p_channel_posts ADD COLUMN IF NOT EXISTS settings_updated BOOLEAN NOT NULL DEFAULT FALSE`,
			})
		},
	})
	return m.Up(ctx)
}

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
