package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentmemory"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

func (s *DatabaseStore) StartKnowledgeUpload(ctx context.Context, owner, filename, mime string, size int64, idem string) (agentmemory.KnowledgeUpload, bool, error) {
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(filename) == "" || idem == "" || size < 0 || size > 10*1024*1024 {
		return agentmemory.KnowledgeUpload{}, false, fmt.Errorf("invalid upload")
	}
	// Cleanup is intentionally bounded to this short-lived raw-upload table.
	_, _ = s.db.ExecContext(ctx, `DELETE FROM p2p_native_agent_knowledge_uploads WHERE ctid IN (SELECT ctid FROM p2p_native_agent_knowledge_uploads WHERE created_at < NOW() - INTERVAL '30 minutes' LIMIT 256)`)
	digest := uploadRequestDigest("knowledge.upload.start", owner, filename, mime, fmt.Sprintf("%d", size))
	var out agentmemory.KnowledgeUpload
	var replay bool
	err := s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		if _, ok, err := knowledgeUploadReceipt(ctx, tx, owner, "knowledge.upload.start", idem, digest, &out); err != nil || ok {
			replay = ok
			return err
		}
		now := time.Now().UTC()
		out = agentmemory.KnowledgeUpload{ID: uuid.NewString(), SourceID: uuid.NewString(), Owner: owner, Filename: filename, MimeType: mime, Size: size, Status: "receiving", CreatedAt: now}
		if _, err := tx.ExecContext(ctx, `INSERT INTO p2p_native_agent_knowledge_uploads(owner_id,upload_id,source_id,filename,mime_type,size,received_size,data,created_at) VALUES($1,$2,$3,$4,$5,$6,0,$7,$8)`, owner, out.ID, out.SourceID, filename, mime, size, []byte{}, now); err != nil {
			return err
		}
		return recordKnowledgeReceipt(ctx, tx, owner, "knowledge.upload.start", idem, digest, out)
	})
	return out, replay, err
}
func (s *DatabaseStore) AppendKnowledgeUpload(ctx context.Context, owner, uploadID string, offset int64, data []byte, idem string, digest [32]byte) (agentmemory.KnowledgeUpload, error) {
	if len(data) > 256*1024 {
		return agentmemory.KnowledgeUpload{}, fmt.Errorf("chunk exceeds 256 KiB")
	}
	if idem == "" || sha256.Sum256(data) != digest {
		return agentmemory.KnowledgeUpload{}, fmt.Errorf("invalid chunk request")
	}
	request := uploadRequestDigest("knowledge.upload.append", owner, uploadID, fmt.Sprintf("%d", offset), fmt.Sprintf("%x", digest))
	var u agentmemory.KnowledgeUpload
	var existing []byte
	err := s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		if _, ok, err := knowledgeUploadReceipt(ctx, tx, owner, "knowledge.upload.append", idem, request, &u); err != nil || ok {
			return err
		}
		err := tx.QueryRowContext(ctx, `SELECT owner_id,source_id,filename,mime_type,size,received_size,data,created_at FROM p2p_native_agent_knowledge_uploads WHERE owner_id=$1 AND upload_id=$2 FOR UPDATE`, owner, uploadID).Scan(&u.Owner, &u.SourceID, &u.Filename, &u.MimeType, &u.Size, &u.Received, &existing, &u.CreatedAt)
		if err != nil {
			return err
		}
		u.ID, u.Status = uploadID, "receiving"
		if offset != u.Received {
			return fmt.Errorf("chunk offset mismatch")
		}
		if u.Received+int64(len(data)) > u.Size {
			return fmt.Errorf("upload exceeds declared size")
		}
		u.Data = append(existing, data...)
		u.Received += int64(len(data))
		_, err = tx.ExecContext(ctx, `UPDATE p2p_native_agent_knowledge_uploads SET received_size=$3,data=$4 WHERE owner_id=$1 AND upload_id=$2`, owner, uploadID, u.Received, u.Data)
		if err != nil {
			return err
		}
		return recordKnowledgeReceipt(ctx, tx, owner, "knowledge.upload.append", idem, request, u)
	})
	return u, err
}
func (s *DatabaseStore) FinishKnowledgeUpload(ctx context.Context, owner, uploadID, title string, digest [32]byte, idem string, openSession agentmemory.KnowledgeEmbeddingSessionFunc) (agentmemory.KnowledgeSource, error) {
	if idem == "" || openSession == nil {
		return agentmemory.KnowledgeSource{}, fmt.Errorf("default embedding profile and idempotency_key are required")
	}
	request := uploadRequestDigest("knowledge.upload.finish", owner, uploadID, title, fmt.Sprintf("%x", digest))
	var replay agentmemory.KnowledgeSource
	if ok, err := readKnowledgeReceipt(ctx, s.db, owner, "knowledge.upload.finish", idem, request, &replay); err != nil {
		return agentmemory.KnowledgeSource{}, err
	} else if ok {
		return replay, nil
	}
	// Read the owner-scoped immutable snapshot before opening a model session; no
	// writer lock or database transaction spans provider/network work.
	var sourceID, mime string
	var size, received int64
	var data []byte
	if err := s.db.QueryRowContext(ctx, `SELECT source_id,mime_type,size,received_size,data FROM p2p_native_agent_knowledge_uploads WHERE owner_id=$1 AND upload_id=$2`, owner, uploadID).Scan(&sourceID, &mime, &size, &received, &data); err != nil {
		return agentmemory.KnowledgeSource{}, err
	}
	if received != size {
		return agentmemory.KnowledgeSource{}, fmt.Errorf("upload is incomplete")
	}
	if sha256.Sum256(data) != digest {
		return agentmemory.KnowledgeSource{}, fmt.Errorf("content_sha256 mismatch")
	}
	if !utf8.Valid(data) {
		return agentmemory.KnowledgeSource{}, fmt.Errorf("upload must be valid UTF-8")
	}
	session, err := openSession(ctx)
	if err != nil || session.Embed == nil {
		if err != nil {
			return agentmemory.KnowledgeSource{}, err
		}
		return agentmemory.KnowledgeSource{}, fmt.Errorf("embedding session is not ready")
	}
	if session.ValidateCurrent != nil {
		if err := session.ValidateCurrent(ctx); err != nil {
			return agentmemory.KnowledgeSource{}, err
		}
	}
	parts := boundedSourceChunks(string(data), 8192)
	type chunk struct {
		text    string
		vector  []float32
		ordinal int64
	}
	chunksData := make([]chunk, 0, len(parts))
	for ordinal, text := range parts {
		if strings.TrimSpace(text) == "" {
			continue
		}
		vector, e := session.Embed(ctx, text)
		if e != nil {
			return agentmemory.KnowledgeSource{}, e
		}
		if !validVector(vector) {
			return agentmemory.KnowledgeSource{}, fmt.Errorf("invalid embedding vector")
		}
		chunksData = append(chunksData, chunk{text: text, vector: vector, ordinal: int64(ordinal)})
	}
	if len(chunksData) == 0 {
		return agentmemory.KnowledgeSource{}, fmt.Errorf("upload contains no text")
	}
	var src agentmemory.KnowledgeSource
	err = s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		if _, ok, err := knowledgeUploadReceipt(ctx, tx, owner, "knowledge.upload.finish", idem, request, &src); err != nil || ok {
			return err
		}
		var currentSource, currentMime string
		var currentSize, currentReceived int64
		var currentData []byte
		if err := tx.QueryRowContext(ctx, `SELECT source_id,mime_type,size,received_size,data FROM p2p_native_agent_knowledge_uploads WHERE owner_id=$1 AND upload_id=$2 FOR UPDATE`, owner, uploadID).Scan(&currentSource, &currentMime, &currentSize, &currentReceived, &currentData); err != nil {
			return err
		}
		if currentSource != sourceID || currentMime != mime || currentSize != size || currentReceived != received || sha256.Sum256(currentData) != digest {
			return fmt.Errorf("upload changed during indexing")
		}
		if session.ValidateCurrent != nil {
			if err := session.ValidateCurrent(ctx); err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		chunks := int64(len(chunksData))
		err = safeSourceInsert(ctx, tx, owner, sourceID, mime, title, size, chunks, now)
		if err != nil {
			return err
		}
		for _, c := range chunksData {
			memoryID := fmt.Sprintf("%s-%d", sourceID, c.ordinal)
			md := agentmemory.KnowledgeDigest(title, c.text, nil)
			tagsJSON := []byte("[]")
			if _, err := tx.ExecContext(ctx, `INSERT INTO p2p_native_agent_memory_records(owner_id,memory_id,title,content,tags_json,request_digest,source_id,chunk_ordinal,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9)`, owner, memoryID, title, c.text, tagsJSON, md[:], sourceID, c.ordinal, now); err != nil {
				return err
			}
			vals := make([]float64, len(c.vector))
			for i, v := range c.vector {
				vals[i] = float64(v)
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO p2p_native_agent_memory_embeddings(owner_id,memory_id,profile_id,profile_revision,model,dimension,content_digest,vector,indexed_at) SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9 WHERE EXISTS (SELECT 1 FROM p2p_agent_model_profile_defaults d JOIN p2p_agent_model_profiles p ON p.owner_id=d.owner_id AND p.profile_id=d.embedding_profile_id WHERE d.owner_id=$1 AND d.embedding_profile_id=$3 AND p.revision=$4 AND p.model=$5 AND p.deleted_at IS NULL)`, owner, memoryID, session.ProfileID, session.Revision, session.Model, len(vals), md[:], pq.Array(vals), now)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			if n == 0 {
				return agentmemory.ErrEmbeddingSessionStale
			}
		}
		src = agentmemory.KnowledgeSource{ID: sourceID, Kind: "upload", Status: "ready", Title: title, MimeType: mime, Size: size, TotalChunks: chunks, IndexedChunks: chunks, Revision: 1, CreatedAt: now, UpdatedAt: now}
		if _, err = tx.ExecContext(ctx, `DELETE FROM p2p_native_agent_knowledge_uploads WHERE owner_id=$1 AND upload_id=$2`, owner, uploadID); err != nil {
			return err
		}
		return recordKnowledgeReceipt(ctx, tx, owner, "knowledge.upload.finish", idem, request, src)
	})
	return src, err
}

func uploadRequestDigest(action string, parts ...string) [32]byte {
	return sha256.Sum256([]byte(strings.Join(append([]string{action}, parts...), "\x00")))
}
func knowledgeUploadReceipt(ctx context.Context, tx *sql.Tx, owner, action, idem string, digest [32]byte, target any) (bool, bool, error) {
	var old, response []byte
	err := tx.QueryRowContext(ctx, `SELECT request_digest,response_json FROM p2p_native_agent_conversation_mutations WHERE owner_id=$1 AND action=$2 AND idempotency_key=$3`, owner, action, idem).Scan(&old, &response)
	if err == sql.ErrNoRows {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if string(old) != string(digest[:]) {
		return false, false, agentmemory.ErrIdempotencyConflict
	}
	if err := json.Unmarshal(response, target); err != nil {
		return false, false, err
	}
	return true, true, nil
}
func recordKnowledgeReceipt(ctx context.Context, tx *sql.Tx, owner, action, idem string, digest [32]byte, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO p2p_native_agent_conversation_mutations(owner_id,action,idempotency_key,request_digest,response_json,created_at) VALUES($1,$2,$3,$4,$5::jsonb,NOW())`, owner, action, idem, digest[:], string(raw))
	return err
}
func boundedSourceChunks(text string, max int) []string {
	out := []string{}
	for _, p := range strings.Split(text, "\n\n") {
		r := []rune(p)
		for len(r) > max {
			out = append(out, string(r[:max]))
			r = r[max:]
		}
		if strings.TrimSpace(string(r)) != "" {
			out = append(out, string(r))
		}
	}
	return out
}
func safeSourceInsert(ctx context.Context, tx *sql.Tx, owner, id, mime, title string, size, chunks int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO p2p_native_agent_knowledge_sources(owner_id,source_id,kind,status,title,mime_type,size,total_chunks,indexed_chunks,revision,created_at,updated_at) VALUES($1,$2,'upload','ready',$3,$4,$5,$6,$6,1,$7,$7)`, owner, id, title, mime, size, chunks, now)
	return err
}
func (s *DatabaseStore) ListKnowledgeSources(ctx context.Context, owner string, limit int, token string) ([]agentmemory.KnowledgeSource, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	start := 0
	if token != "" {
		if _, err := fmt.Sscanf(token, "%d", &start); err != nil || start < 0 {
			return nil, "", agentmemory.ErrInvalidCursor
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT source_id,kind,status,title,mime_type,size,total_chunks,indexed_chunks,revision,error_text,created_at,updated_at FROM p2p_native_agent_knowledge_sources WHERE owner_id=$1 ORDER BY created_at ASC,source_id ASC OFFSET $2 LIMIT $3`, owner, start, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []agentmemory.KnowledgeSource{}
	for rows.Next() {
		var x agentmemory.KnowledgeSource
		if err := rows.Scan(&x.ID, &x.Kind, &x.Status, &x.Title, &x.MimeType, &x.Size, &x.TotalChunks, &x.IndexedChunks, &x.Revision, &x.Error, &x.CreatedAt, &x.UpdatedAt); err != nil {
			return nil, "", err
		}
		out = append(out, x)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = fmt.Sprintf("%d", start+limit)
	}
	return out, next, rows.Err()
}
func (s *DatabaseStore) DeleteKnowledgeSource(ctx context.Context, owner, id string, expected int64, idem string, digest [32]byte) (agentmemory.KnowledgeSource, bool, error) {
	var x agentmemory.KnowledgeSource
	var replay bool
	err := s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		if _, ok, err := knowledgeUploadReceipt(ctx, tx, owner, "knowledge.source.delete", idem, digest, &x); err != nil || ok {
			replay = ok
			return err
		}
		err := tx.QueryRowContext(ctx, `DELETE FROM p2p_native_agent_knowledge_sources WHERE owner_id=$1 AND source_id=$2 AND revision=$3 RETURNING source_id,kind,status,title,mime_type,size,total_chunks,indexed_chunks,revision,error_text,created_at,updated_at`, owner, id, expected).Scan(&x.ID, &x.Kind, &x.Status, &x.Title, &x.MimeType, &x.Size, &x.TotalChunks, &x.IndexedChunks, &x.Revision, &x.Error, &x.CreatedAt, &x.UpdatedAt)
		if err == sql.ErrNoRows {
			var revision int64
			if lookupErr := tx.QueryRowContext(ctx, `SELECT revision FROM p2p_native_agent_knowledge_sources WHERE owner_id=$1 AND source_id=$2`, owner, id).Scan(&revision); lookupErr == sql.ErrNoRows {
				return agentmemory.ErrNotFound
			} else if lookupErr != nil {
				return lookupErr
			}
			return agentmemory.ErrRevisionConflict
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM p2p_native_agent_memory_records WHERE owner_id=$1 AND source_id=$2`, owner, id)
			if err == nil {
				err = recordKnowledgeReceipt(ctx, tx, owner, "knowledge.source.delete", idem, digest, x)
			}
		}
		return err
	})
	return x, replay, err
}

func readKnowledgeReceipt(ctx context.Context, db *sql.DB, owner, action, idem string, digest [32]byte, target any) (bool, error) {
	var old, response []byte
	err := db.QueryRowContext(ctx, `SELECT request_digest,response_json FROM p2p_native_agent_conversation_mutations WHERE owner_id=$1 AND action=$2 AND idempotency_key=$3`, owner, action, idem).Scan(&old, &response)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if string(old) != string(digest[:]) {
		return false, agentmemory.ErrIdempotencyConflict
	}
	return true, json.Unmarshal(response, target)
}
