package leads

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/ashmit/reachout-server/internal/accounts"
	"github.com/ashmit/reachout-server/internal/linkedin"
	"github.com/ashmit/reachout-server/internal/mdb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
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
	db  *mongo.Database
	li  linkedin.Client
	acc Accounts
	log *slog.Logger
	now func() time.Time

	// MaxLeads is the per-user lead limit (MaxLeadsPerUser unless a test lowers it).
	MaxLeads int
}

func NewService(db *mongo.Database, li linkedin.Client, acc Accounts, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: db, li: li, acc: acc, log: log, now: time.Now, MaxLeads: MaxLeadsPerUser}
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

type candidate struct{ url, name, headline, urn string }

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
	added, err := s.insert(ctx, userID, cands)
	res.Imported += added.Imported
	res.Duplicates += added.Duplicates
	res.OverCap += added.OverCap
	return res, err
}

// leadDoc is the stored lead.
type leadDoc struct {
	ID          int64     `bson:"_id"`
	UserID      int64     `bson:"user_id"`
	ProfileURL  string    `bson:"profile_url"`
	Name        string    `bson:"name"`
	Headline    string    `bson:"headline"`
	Company     string    `bson:"company"`
	CreatedAt   time.Time `bson:"created_at"`
	LinkedInURN *string   `bson:"linkedin_urn,omitempty"`
	HashedURL   *string   `bson:"hashed_url,omitempty"`
}

func (d leadDoc) lead() Lead {
	return Lead{ID: d.ID, ProfileURL: d.ProfileURL, Name: d.Name, Headline: d.Headline, Company: d.Company, CreatedAt: d.CreatedAt}
}

func (s *Service) coll() *mongo.Collection { return s.db.Collection(mdb.Leads) }

func eq(k string, v any) bson.E { return bson.E{Key: k, Value: v} }

// inTx runs fn in a transaction that first takes a per-user lock, so two imports for the
// same user cannot both pass the lead-limit check and exceed it. fn may run more than once
// (the transaction is retried on a conflict), so it must not carry state between attempts.
func (s *Service) inTx(ctx context.Context, userID int64, fn func(ctx context.Context) error) error {
	return mdb.WithTx(ctx, s.db, func(ctx context.Context) error {
		if err := mdb.Lock(ctx, s.db.Collection(mdb.Counters), bson.D{eq("_id", fmt.Sprintf("leads:%d", userID))}); err != nil {
			return err
		}
		return fn(ctx)
	})
}

// insert adds the candidates in a transaction of its own. Ids are reserved up front:
// allocating inside the transaction would make every import queue on the counter document.
func (s *Service) insert(ctx context.Context, userID int64, cands []candidate) (ImportResult, error) {
	if len(cands) == 0 {
		return ImportResult{}, nil
	}
	first, err := mdb.NextIDs(ctx, s.db, mdb.Leads, len(cands))
	if err != nil {
		return ImportResult{}, err
	}
	var out ImportResult
	err = s.inTx(ctx, userID, func(ctx context.Context) (err error) {
		out, err = s.insertTx(ctx, userID, cands, first)
		return err
	})
	return out, err
}

// insertTx upserts candidates for userID inside a transaction the caller has opened with inTx
// (so the per-user lock is held), using ids first, first+1, ... Existing leads keep their data,
// but blank name, headline and URN fields are filled from the new import.
func (s *Service) insertTx(ctx context.Context, userID int64, cands []candidate, first int64) (ImportResult, error) {
	var out ImportResult
	{
		total, err := s.coll().CountDocuments(ctx, bson.D{eq("user_id", userID)})
		if err != nil {
			return out, err
		}
		now := s.now()
		for i, c := range cands {
			key := bson.D{eq("user_id", userID), eq("profile_url", c.url)}
			if int(total) >= s.MaxLeads {
				// At the limit only existing leads can be touched; anything new is skipped.
				n, err := s.coll().CountDocuments(ctx, key)
				if err != nil {
					return out, err
				}
				if n > 0 {
					out.Duplicates++
				} else {
					out.OverCap++
				}
				continue
			}
			setOnInsert := bson.D{eq("_id", first+int64(i)), eq("created_at", now), eq("company", "")}
			// name, headline and URN are set on insert only when given; blanks on an existing lead are filled below.
			setOnInsert = append(setOnInsert, eq("name", c.name), eq("headline", c.headline))
			if c.urn != "" {
				setOnInsert = append(setOnInsert, eq("linkedin_urn", c.urn))
			}
			r, err := s.coll().UpdateOne(ctx, key, bson.D{{Key: "$setOnInsert", Value: setOnInsert}}, options.UpdateOne().SetUpsert(true))
			if err != nil {
				return out, err
			}
			if r.UpsertedCount == 1 {
				out.Imported++
				total++
				continue
			}
			out.Duplicates++
			for field, v := range map[string]string{"name": c.name, "headline": c.headline} {
				if v != "" {
					if _, err := s.coll().UpdateOne(ctx, append(bson.D{}, append(key, eq(field, ""))...), bson.D{{Key: "$set", Value: bson.D{eq(field, v)}}}); err != nil {
						return out, err
					}
				}
			}
			if c.urn != "" {
				if _, err := s.coll().UpdateOne(ctx, append(bson.D{}, append(key, eq("linkedin_urn", bson.D{{Key: "$exists", Value: false}}))...),
					bson.D{{Key: "$set", Value: bson.D{eq("linkedin_urn", c.urn)}}}); err != nil {
					return out, err
				}
			}
		}
	}
	return out, nil
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
	// The search text is matched literally (QuoteMeta), case-insensitively, anywhere in the fields.
	re := bson.Regex{Pattern: regexp.QuoteMeta(strings.TrimSpace(p.Query)), Options: "i"}
	filter := bson.D{eq("user_id", userID), {Key: "$or", Value: bson.A{
		bson.D{eq("name", re)}, bson.D{eq("headline", re)}, bson.D{eq("company", re)}, bson.D{eq("profile_url", re)},
	}}}

	page := Page{Leads: []Lead{}, Limit: p.Limit, Offset: p.Offset}
	total, err := s.coll().CountDocuments(ctx, filter)
	if err != nil {
		return Page{}, err
	}
	page.Total = int(total)
	cur, err := s.coll().Find(ctx, filter, options.Find().SetSort(bson.D{eq("_id", -1)}).SetSkip(int64(p.Offset)).SetLimit(int64(p.Limit)))
	if err != nil {
		return Page{}, err
	}
	var docs []leadDoc
	if err := cur.All(ctx, &docs); err != nil {
		return Page{}, err
	}
	for _, d := range docs {
		page.Leads = append(page.Leads, d.lead())
	}
	return page, nil
}

func (s *Service) Get(ctx context.Context, userID, id int64) (Lead, error) {
	var d leadDoc
	err := s.coll().FindOne(ctx, bson.D{eq("_id", id), eq("user_id", userID)}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Lead{}, ErrNotFound
	}
	return d.lead(), err
}

// inActiveCampaign returns which of the given leads sit in a running or paused campaign.
// Deleting such a lead would silently pull a prospect out of a live sequence, so it is
// refused: remove the lead from the campaign or stop the campaign first.
func (s *Service) inActiveCampaign(ctx context.Context, leadIDs []int64) (map[int64]bool, error) {
	cur, err := s.db.Collection(mdb.CampaignLeads).Find(ctx, bson.D{eq("lead_id", bson.D{{Key: "$in", Value: leadIDs}})})
	if err != nil {
		return nil, err
	}
	var rows []struct {
		CampaignID int64 `bson:"campaign_id"`
		LeadID     int64 `bson:"lead_id"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	var campaignIDs []int64
	for _, r := range rows {
		campaignIDs = append(campaignIDs, r.CampaignID)
	}
	active := map[int64]bool{}
	cc, err := s.db.Collection(mdb.Campaigns).Find(ctx, bson.D{
		eq("_id", bson.D{{Key: "$in", Value: campaignIDs}}), eq("status", bson.D{{Key: "$in", Value: bson.A{"running", "paused"}}}),
	}, options.Find().SetProjection(bson.D{eq("_id", 1)}))
	if err != nil {
		return nil, err
	}
	var live []struct {
		ID int64 `bson:"_id"`
	}
	if err := cc.All(ctx, &live); err != nil {
		return nil, err
	}
	liveSet := map[int64]bool{}
	for _, c := range live {
		liveSet[c.ID] = true
	}
	for _, r := range rows {
		if liveSet[r.CampaignID] {
			active[r.LeadID] = true
		}
	}
	return active, nil
}

// remove deletes the leads and what hung off them (campaign entries in drafts, stopped and
// finished campaigns, and their messages). MongoDB has no cascading deletes, so it is done here.
func (s *Service) remove(ctx context.Context, userID int64, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	in := bson.D{{Key: "$in", Value: ids}}
	r, err := s.coll().DeleteMany(ctx, bson.D{eq("_id", in), eq("user_id", userID)})
	if err != nil {
		return 0, err
	}
	if r.DeletedCount > 0 {
		if _, err := s.db.Collection(mdb.CampaignLeads).DeleteMany(ctx, bson.D{eq("lead_id", in), eq("user_id", userID)}); err != nil {
			return r.DeletedCount, err
		}
		if _, err := s.db.Collection(mdb.Messages).DeleteMany(ctx, bson.D{eq("lead_id", in)}); err != nil {
			return r.DeletedCount, err
		}
	}
	return r.DeletedCount, nil
}

// Delete removes one lead. Campaign entries in drafts, stopped and finished campaigns go
// with it; a lead in a running or paused campaign is refused.
func (s *Service) Delete(ctx context.Context, userID, id int64) error {
	if _, err := s.Get(ctx, userID, id); err != nil {
		return err // not found, or not this user's
	}
	active, err := s.inActiveCampaign(ctx, []int64{id})
	if err != nil {
		return err
	}
	if active[id] {
		return ErrInActiveCampaign
	}
	_, err = s.remove(ctx, userID, []int64{id})
	return err
}

// DeleteMany removes the listed leads that belong to userID. It returns how many were
// deleted and how many were skipped because they are in a running or paused campaign.
// Ids that are unknown or owned by someone else are ignored.
func (s *Service) DeleteMany(ctx context.Context, userID int64, ids []int64) (deleted, blocked int64, err error) {
	if len(ids) == 0 || len(ids) > MaxBulkDelete {
		return 0, 0, fmt.Errorf("%w: send between 1 and %d ids", ErrInvalidInput, MaxBulkDelete)
	}
	cur, err := s.coll().Find(ctx, bson.D{eq("_id", bson.D{{Key: "$in", Value: ids}}), eq("user_id", userID)},
		options.Find().SetProjection(bson.D{eq("_id", 1)}))
	if err != nil {
		return 0, 0, err
	}
	var owned []struct {
		ID int64 `bson:"_id"`
	}
	if err := cur.All(ctx, &owned); err != nil {
		return 0, 0, err
	}
	ownedIDs := make([]int64, 0, len(owned))
	for _, o := range owned {
		ownedIDs = append(ownedIDs, o.ID)
	}
	if len(ownedIDs) == 0 {
		return 0, 0, nil
	}
	active, err := s.inActiveCampaign(ctx, ownedIDs)
	if err != nil {
		return 0, 0, err
	}
	var deletable []int64
	for _, id := range ownedIDs {
		if active[id] {
			blocked++
		} else {
			deletable = append(deletable, id)
		}
	}
	deleted, err = s.remove(ctx, userID, deletable)
	return deleted, blocked, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
