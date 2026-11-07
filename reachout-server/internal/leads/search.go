package leads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ashmit/reachout-server/internal/accounts"
	"github.com/ashmit/reachout-server/internal/linkedin"
	"github.com/ashmit/reachout-server/internal/mdb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// importTTL is how long a search import may stay running. Linked API workflows normally
// finish in under a few minutes; past this we give up so the account is not stuck.
const importTTL = 20 * time.Minute

// Import is one search import, as shown to the browser.
type Import struct {
	ID            int64      `json:"id"`
	AccountID     int64      `json:"account_id"`
	SearchURL     string     `json:"search_url"`
	Requested     int        `json:"requested"`
	Status        string     `json:"status"` // running | completed | failed
	Found         int        `json:"found"`
	Imported      int        `json:"imported"`
	Duplicates    int        `json:"duplicates"`
	Invalid       int        `json:"invalid"`
	OverCap       int        `json:"over_cap"`
	Message       string     `json:"message,omitempty"`
	PendingReason string     `json:"pending_reason,omitempty"` // while running: queued | outsideWorkingHours
	CreatedAt     time.Time  `json:"created_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
}

// importDoc is the stored import.
type importDoc struct {
	ID         int64      `bson:"_id"`
	UserID     int64      `bson:"user_id"`
	AccountID  int64      `bson:"account_id"`
	SearchURL  string     `bson:"search_url"`
	Requested  int        `bson:"requested"`
	Status     string     `bson:"status"`
	WorkflowID *string    `bson:"workflow_id,omitempty"`
	Found      int        `bson:"found"`
	Imported   int        `bson:"imported"`
	Duplicates int        `bson:"duplicates"`
	Invalid    int        `bson:"invalid"`
	OverCap    int        `bson:"over_cap"`
	Message    string     `bson:"message"`
	CreatedAt  time.Time  `bson:"created_at"`
	FinishedAt *time.Time `bson:"finished_at,omitempty"`
}

func (d importDoc) imp() Import {
	return Import{ID: d.ID, AccountID: d.AccountID, SearchURL: d.SearchURL, Requested: d.Requested, Status: d.Status,
		Found: d.Found, Imported: d.Imported, Duplicates: d.Duplicates, Invalid: d.Invalid, OverCap: d.OverCap,
		Message: d.Message, CreatedAt: d.CreatedAt, FinishedAt: d.FinishedAt}
}

func (s *Service) imports() *mongo.Collection { return s.db.Collection(mdb.LeadImports) }

// OutsideWorkingHoursError carries when the account may next run a search.
type OutsideWorkingHoursError struct{ Next time.Time }

func (e *OutsideWorkingHoursError) Error() string {
	return "searches run during the account's working hours; next window opens " + e.Next.Format(time.RFC3339)
}
func (e *OutsideWorkingHoursError) Unwrap() error { return ErrOutsideWorkingHrs }

// StartSearch begins importing people from a LinkedIn people-search URL.
//
// A search is activity on the user's LinkedIn account, so it follows the same weekday and
// working-hours rules as sending: outside the window it is refused with the next window.
// Only one import may run per account, since Linked API runs one workflow at a time.
func (s *Service) StartSearch(ctx context.Context, userID, accountID int64, searchURL string, limit int) (Import, error) {
	searchURL, err := ValidateSearchURL(searchURL)
	if err != nil {
		return Import{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if limit == 0 {
		limit = DefaultSearchLim
	}
	if limit < 1 || limit > MaxSearchLimit {
		return Import{}, fmt.Errorf("%w: limit must be 1 to %d", ErrInvalidInput, MaxSearchLimit)
	}

	acct, err := s.acc.Get(ctx, userID, accountID) // 404 for another user's account
	if err != nil {
		return Import{}, err
	}
	if acct.Status != accounts.StatusConnected {
		return Import{}, ErrAccountNotReady
	}
	sched, err := s.acc.GetSchedule(ctx, userID, accountID)
	if err != nil {
		return Import{}, err
	}
	if !sched.OpenNow {
		return Import{}, &OutsideWorkingHoursError{Next: sched.NextSendWindow}
	}
	token, err := s.acc.IdentificationToken(ctx, accountID)
	if err != nil {
		return Import{}, err
	}

	id, err := mdb.NextID(ctx, s.db, mdb.LeadImports)
	if err != nil {
		return Import{}, err
	}
	// The one_running_import_per_acct index lets only one running import exist per account.
	if _, err := s.imports().InsertOne(ctx, importDoc{
		ID: id, UserID: userID, AccountID: accountID, SearchURL: searchURL, Requested: limit,
		Status: "running", CreatedAt: s.now(),
	}); err != nil {
		if mdb.IsDup(err) {
			return Import{}, ErrImportInProgress
		}
		return Import{}, err
	}

	ws, err := s.li.StartWorkflow(ctx, token, map[string]any{
		"actionType":      "st.searchPeople",
		"customSearchUrl": searchURL,
		"limit":           limit,
	})
	if err != nil {
		s.finish(ctx, id, userID, "failed", friendlyError(err))
		return Import{}, err
	}
	if _, err := s.imports().UpdateOne(ctx, bson.D{eq("_id", id)}, bson.D{{Key: "$set", Value: bson.D{eq("workflow_id", ws.ID)}}}); err != nil {
		return Import{}, err
	}
	imp, err := s.loadImport(ctx, userID, id)
	imp.PendingReason = ws.PendingReason
	return imp, err
}

func (s *Service) loadDoc(ctx context.Context, userID, id int64) (importDoc, error) {
	var d importDoc
	err := s.imports().FindOne(ctx, bson.D{eq("_id", id), eq("user_id", userID)}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return importDoc{}, ErrNotFound
	}
	return d, err
}

func (s *Service) loadImport(ctx context.Context, userID, id int64) (Import, error) {
	d, err := s.loadDoc(ctx, userID, id)
	return d.imp(), err
}

// GetImport returns an import and, while it is running, checks Linked API for its result.
// Several browsers polling at once is safe: only one of them completes the import.
func (s *Service) GetImport(ctx context.Context, userID, id int64) (Import, error) {
	doc, err := s.loadDoc(ctx, userID, id)
	imp := doc.imp()
	if err != nil || imp.Status != "running" {
		return imp, err
	}
	if s.now().Sub(imp.CreatedAt) > importTTL {
		s.finish(ctx, id, userID, "failed", "the search took too long and was abandoned")
		return s.loadImport(ctx, userID, id)
	}
	if doc.WorkflowID == nil {
		return imp, nil // still being submitted
	}
	workflowID := *doc.WorkflowID
	token, err := s.acc.IdentificationToken(ctx, imp.AccountID)
	if err != nil {
		return Import{}, err
	}
	res, err := s.li.GetWorkflow(ctx, token, workflowID)
	if err != nil {
		return Import{}, err
	}
	switch res.Status {
	case "completed":
		return s.applySearch(ctx, userID, imp, res.Completion)
	case "failed":
		msg := "the search failed"
		if res.Failure != nil {
			msg = failureMessage(res.Failure)
		}
		s.finish(ctx, id, userID, "failed", msg)
		return s.loadImport(ctx, userID, id)
	default:
		imp.PendingReason = res.PendingReason
		return imp, nil
	}
}

// searchItem is one person in a st.searchPeople result.
type searchItem struct {
	Name      string  `json:"name"`
	URN       *string `json:"urn"`
	PublicURL *string `json:"publicUrl"`
	Headline  string  `json:"headline"`
}

func (s *Service) applySearch(ctx context.Context, userID int64, imp Import, completion json.RawMessage) (Import, error) {
	var comp struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
		Error   *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(completion, &comp); err != nil {
		s.log.Error("search result not JSON", "import", imp.ID, "err", err)
		s.finish(ctx, imp.ID, userID, "failed", "unexpected result from LinkedIn")
		return s.loadImport(ctx, userID, imp.ID)
	}
	if !comp.Success {
		msg := "the search failed"
		if comp.Error != nil {
			msg = actionErrorMessage(comp.Error.Type, comp.Error.Message)
		}
		s.finish(ctx, imp.ID, userID, "failed", msg)
		return s.loadImport(ctx, userID, imp.ID)
	}
	var items []searchItem
	if err := json.Unmarshal(comp.Data, &items); err != nil {
		s.log.Error("search result shape", "import", imp.ID, "err", err)
		s.finish(ctx, imp.ID, userID, "failed", "unexpected result from LinkedIn")
		return s.loadImport(ctx, userID, imp.ID)
	}

	res := ImportResult{}
	seen := map[string]bool{}
	var cands []candidate
	for _, it := range items {
		if it.PublicURL == nil {
			res.Invalid = append(res.Invalid, InvalidURL{})
			continue
		}
		n, err := NormalizeProfileURL(*it.PublicURL)
		if err != nil {
			res.Invalid = append(res.Invalid, InvalidURL{})
			continue
		}
		if seen[n] {
			res.Duplicates++
			continue
		}
		seen[n] = true
		urn := ""
		if it.URN != nil {
			urn = truncate(*it.URN, 200)
		}
		cands = append(cands, candidate{url: n, name: truncate(it.Name, 200), headline: truncate(it.Headline, 500), urn: urn})
	}

	var first int64
	if len(cands) > 0 {
		var err error
		if first, err = mdb.NextIDs(ctx, s.db, mdb.Leads, len(cands)); err != nil {
			return Import{}, err
		}
	}
	err := s.inTx(ctx, userID, func(ctx context.Context) error {
		// Claim the import first: if another poller already completed it, do nothing. The
		// claim and the new leads commit together, so a crash cannot leave a finished import
		// with no leads.
		r, err := s.imports().UpdateOne(ctx, bson.D{eq("_id", imp.ID), eq("user_id", userID), eq("status", "running")},
			bson.D{{Key: "$set", Value: bson.D{eq("status", "completed"), eq("finished_at", s.now())}}})
		if err != nil {
			return err
		}
		if r.ModifiedCount == 0 {
			return nil
		}
		added, err := s.insertTx(ctx, userID, cands, first)
		if err != nil {
			return err
		}
		_, err = s.imports().UpdateOne(ctx, bson.D{eq("_id", imp.ID)}, bson.D{{Key: "$set", Value: bson.D{
			eq("found", len(items)), eq("imported", added.Imported), eq("duplicates", res.Duplicates+added.Duplicates),
			eq("invalid", len(res.Invalid)), eq("over_cap", added.OverCap),
		}}})
		return err
	})
	if err != nil {
		return Import{}, err
	}
	return s.loadImport(ctx, userID, imp.ID)
}

func (s *Service) finish(ctx context.Context, id, userID int64, status, message string) {
	if _, err := s.imports().UpdateOne(ctx, bson.D{eq("_id", id), eq("user_id", userID), eq("status", "running")},
		bson.D{{Key: "$set", Value: bson.D{eq("status", status), eq("message", message), eq("finished_at", s.now())}}}); err != nil {
		s.log.Error("finish import", "import", id, "err", err)
	}
}

func friendlyError(err error) string {
	switch {
	case linkedin.IsSignedOut(err):
		return "the LinkedIn account is signed out, reconnect it and try again"
	case linkedin.IsLimitHit(err):
		return "the LinkedIn search limit was reached, try again later"
	case linkedin.IsOutsideWorkingHours(err):
		return "the account is outside its Linked API working hours"
	case linkedin.IsTransient(err):
		return "LinkedIn service is unavailable, try again shortly"
	}
	return "the search could not be started"
}

func failureMessage(f *linkedin.WorkflowFailure) string {
	switch f.Reason {
	case "linkedinAccountSignedOut":
		return "the LinkedIn account is signed out, reconnect it and try again"
	case "languageNotSupported":
		return "the LinkedIn account language is not supported (use English, German, Spanish, French or Portuguese)"
	}
	return "the search failed"
}

func actionErrorMessage(typ, _ string) string {
	switch typ {
	case "searchingNotAllowed":
		return "LinkedIn did not allow this search right now, try again later"
	case "limitExceeded":
		return "the search limit for this account was reached, try again later"
	case "filterNotApplied":
		return "one of the filters in the search URL could not be applied"
	}
	return "the search failed"
}
