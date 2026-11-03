package leads

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ashmit/reachout-server/internal/accounts"
	"github.com/ashmit/reachout-server/internal/linkedin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	MaxPasteURLs     = 500 // profile URLs accepted in one paste
	MaxSearchLimit   = 100 // people requested from one search import
	DefaultSearchLim = 25
	MaxLeadsPerUser  = 5000 // total leads one user may hold
	MaxBulkDelete    = 500
	MaxPageSize      = 200
	DefaultPageSize  = 50
)

var (
	ErrNotFound          = errors.New("lead not found")
	ErrInActiveCampaign  = errors.New("lead is in a running or paused campaign; remove it from the campaign or stop the campaign first")
	ErrInvalidInput      = errors.New("invalid input")
	ErrAccountNotReady   = errors.New("LinkedIn account is not connected")
	ErrImportInProgress  = errors.New("a search import is already running for this account")
	ErrOutsideWorkingHrs = errors.New("outside working hours")
)

// Accounts is the part of accounts.Service this package needs. Every method that takes
// a userID enforces ownership, so a foreign account id looks like a missing one.
type Accounts interface {
	Get(ctx context.Context, userID, id int64) (accounts.Account, error)
	GetSchedule(ctx context.Context, userID, id int64) (accounts.ScheduleView, error)
	IdentificationToken(ctx context.Context, accountID int64) (string, error)
}

type Lead struct {
	ID         int64     `json:"id"`
	ProfileURL string    `json:"profile_url"`
	Name       string    `json:"name"`
	Headline   string    `json:"headline"`
	Company    string    `json:"company"`
	CreatedAt  time.Time `json:"created_at"`
}

type Service struct {
	pool *pgxpool.Pool
	li   linkedin.Client
	acc  Accounts
	log  *slog.Logger
	now  func() time.Time

	// MaxLeads is the per-user lead limit (MaxLeadsPerUser unless a test lowers it).
	MaxLeads int
}

func NewService(pool *pgxpool.Pool, li linkedin.Client, acc Accounts, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: pool, li: li, acc: acc, log: log, now: time.Now, MaxLeads: MaxLeadsPerUser}
}

type InvalidURL struct {
	Input  string `json:"input"`
	Reason string `json:"reason"`
}

// ImportResult says what happened to every entry of an import.
type ImportResult struct {
	Imported   int          `json:"imported"`
	Duplicates int          `json:"duplicates"`
	OverCap    int          `json:"over_cap"` // skipped because the per-user lead limit was reached
	Invalid    []InvalidURL `json:"invalid"`
}

type candidate struct{ url, name, headline string }

// ImportURLs adds the profile URLs found in the pasted text. Different spellings of the
// same profile count once; people already in the user's leads count as duplicates.
func (s *Service) ImportURLs(ctx context.Context, userID int64, text string, urls []string) (ImportResult, error) {
	raw := append(splitPasted(text), urls...)
	if len(raw) == 0 {
		return ImportResult{}, fmt.Errorf("%w: no URLs provided", ErrInvalidInput)
	}
	if len(raw) > MaxPasteURLs {
		return ImportResult{}, fmt.Errorf("%w: at most %d URLs per import", ErrInvalidInput, MaxPasteURLs)
	}
	res := ImportResult{Invalid: []InvalidURL{}}
	seen := map[string]bool{}
	var cands []candidate
	for _, in := range raw {
		n, err := NormalizeProfileURL(in)
		if err != nil {
			res.Invalid = append(res.Invalid, InvalidURL{Input: truncate(in, 200), Reason: err.Error()})
			continue
		}
		if seen[n] {
			res.Duplicates++
			continue
		}
		seen[n] = true
		cands = append(cands, candidate{url: n})
	}
	err := s.inTx(ctx, userID, func(tx pgx.Tx) error { return s.insert(ctx, tx, userID, cands, &res) })
	return res, err
}

// inTx runs fn in a transaction holding a per-user lock, so two imports for the same user
// cannot both pass the lead-limit check and exceed it.
func (s *Service) inTx(ctx context.Context, userID int64, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7264520, $1)`, int32(userID)); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// insert upserts candidates for userID and adds the outcome to res. Existing leads keep
// their data, but blank name/headline fields are filled from the new import.
func (s *Service) insert(ctx context.Context, tx pgx.Tx, userID int64, cands []candidate, res *ImportResult) error {
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM leads WHERE user_id = $1`, userID).Scan(&total); err != nil {
		return err
	}
	for _, c := range cands {
		if total >= s.MaxLeads {
			// At the limit only existing leads can be touched; anything new is skipped.
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM leads WHERE user_id = $1 AND profile_url = $2)`,
				userID, c.url).Scan(&exists); err != nil {
				return err
			}
			if exists {
				res.Duplicates++
			} else {
				res.OverCap++
			}
			continue
		}
		var inserted bool
		err := tx.QueryRow(ctx,
			`INSERT INTO leads (user_id, profile_url, name, headline) VALUES ($1, $2, $3, $4)
			 ON CONFLICT (user_id, profile_url) DO UPDATE
			   SET name     = CASE WHEN leads.name = ''     THEN EXCLUDED.name     ELSE leads.name END,
			       headline = CASE WHEN leads.headline = '' THEN EXCLUDED.headline ELSE leads.headline END
			 RETURNING (xmax = 0)`,
			userID, c.url, c.name, c.headline).Scan(&inserted)
		if err != nil {
			return err
		}
		if inserted {
			res.Imported++
			total++
		} else {
			res.Duplicates++
		}
	}
	return nil
}

// ListParams filters and pages the lead list.
type ListParams struct {
	Query  string // matches name, headline, company or URL, case-insensitive
	Limit  int
	Offset int
}

type Page struct {
	Leads  []Lead `json:"leads"`
	Total  int    `json:"total"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (s *Service) List(ctx context.Context, userID int64, p ListParams) (Page, error) {
	if p.Limit <= 0 {
		p.Limit = DefaultPageSize
	}
	if p.Limit > MaxPageSize {
		p.Limit = MaxPageSize
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	pattern := "%" + escapeLike(strings.TrimSpace(p.Query)) + "%"
	const where = `user_id = $1 AND (name ILIKE $2 ESCAPE '\' OR headline ILIKE $2 ESCAPE '\'
	                OR company ILIKE $2 ESCAPE '\' OR profile_url ILIKE $2 ESCAPE '\')`

	page := Page{Leads: []Lead{}, Limit: p.Limit, Offset: p.Offset}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM leads WHERE `+where, userID, pattern).Scan(&page.Total); err != nil {
		return Page{}, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, profile_url, name, headline, company, created_at FROM leads
		  WHERE `+where+` ORDER BY id DESC LIMIT $3 OFFSET $4`, userID, pattern, p.Limit, p.Offset)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var l Lead
		if err := rows.Scan(&l.ID, &l.ProfileURL, &l.Name, &l.Headline, &l.Company, &l.CreatedAt); err != nil {
			return Page{}, err
		}
		page.Leads = append(page.Leads, l)
	}
	return page, rows.Err()
}

func (s *Service) Get(ctx context.Context, userID, id int64) (Lead, error) {
	var l Lead
	err := s.pool.QueryRow(ctx,
		`SELECT id, profile_url, name, headline, company, created_at FROM leads WHERE id = $1 AND user_id = $2`,
		id, userID).Scan(&l.ID, &l.ProfileURL, &l.Name, &l.Headline, &l.Company, &l.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lead{}, ErrNotFound
	}
	return l, err
}

// inActiveCampaign is true for a leads row l that sits in a running or paused campaign.
// Deleting such a lead would silently pull a prospect out of a live sequence, so it is
// refused: remove the lead from the campaign or stop the campaign first.
const inActiveCampaign = `EXISTS (SELECT 1 FROM campaign_leads cl JOIN campaigns c ON c.id = cl.campaign_id
	WHERE cl.lead_id = l.id AND c.status IN ('running','paused'))`

// Delete removes one lead. Campaign entries in drafts, stopped and finished campaigns go
// with it (ON DELETE CASCADE); a lead in a running or paused campaign is refused.
func (s *Service) Delete(ctx context.Context, userID, id int64) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM leads l WHERE l.id = $1 AND l.user_id = $2 AND NOT `+inActiveCampaign, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	if _, err := s.Get(ctx, userID, id); err != nil {
		return err // not found
	}
	return ErrInActiveCampaign
}

// DeleteMany removes the listed leads that belong to userID. It returns how many were
// deleted and how many were skipped because they are in a running or paused campaign.
// Ids that are unknown or owned by someone else are ignored.
func (s *Service) DeleteMany(ctx context.Context, userID int64, ids []int64) (deleted, blocked int64, err error) {
	if len(ids) == 0 || len(ids) > MaxBulkDelete {
		return 0, 0, fmt.Errorf("%w: send between 1 and %d ids", ErrInvalidInput, MaxBulkDelete)
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM leads l WHERE l.user_id = $1 AND l.id = ANY($2) AND NOT `+inActiveCampaign, userID, ids)
	if err != nil {
		return 0, 0, err
	}
	err = s.pool.QueryRow(ctx,
		`SELECT count(*) FROM leads l WHERE l.user_id = $1 AND l.id = ANY($2) AND `+inActiveCampaign, userID, ids).Scan(&blocked)
	return tag.RowsAffected(), blocked, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
