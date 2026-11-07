package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ashmit/reachout-server/internal/mdb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

const SessionTTL = 14 * 24 * time.Hour

var (
	ErrInvalidEmail       = errors.New("invalid email address")
	ErrWeakPassword       = errors.New("password must be 10 to 128 characters")
	ErrEmailTaken         = errors.New("email already registered")
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrNoSession          = errors.New("no valid session")
)

type User struct {
	ID        int64     `json:"id"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

// Service is the database side of auth. It knows nothing about HTTP.
type Service struct {
	db  *mongo.Database
	now func() time.Time
}

func NewService(db *mongo.Database) *Service {
	return &Service{db: db, now: time.Now}
}

type userDoc struct {
	ID           int64     `bson:"_id"`
	Email        string    `bson:"email"`
	PasswordHash string    `bson:"password_hash"`
	CreatedAt    time.Time `bson:"created_at"`
}

func (d userDoc) user() User { return User{ID: d.ID, Email: d.Email, CreatedAt: d.CreatedAt} }

// sessionDoc's id is the SHA-256 of the session token.
type sessionDoc struct {
	ID        []byte    `bson:"_id"`
	UserID    int64     `bson:"user_id"`
	ExpiresAt time.Time `bson:"expires_at"`
	CreatedAt time.Time `bson:"created_at"`
}

// NormalizeEmail lowercases and validates a bare address (no display name).
func NormalizeEmail(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > 254 {
		return "", ErrInvalidEmail
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || !strings.Contains(s[strings.LastIndex(s, "@"):], ".") {
		return "", ErrInvalidEmail
	}
	return s, nil
}

func checkPassword(p string) error {
	if n := utf8.RuneCountInString(p); n < MinPasswordLen || len(p) > MaxPasswordLen*4 || n > MaxPasswordLen {
		return ErrWeakPassword
	}
	return nil
}

// Session tokens are 32 random bytes. Only their SHA-256 is stored, so a database
// leak does not hand out usable sessions.
func newToken() (token string, hash []byte, err error) {
	var b [32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return
	}
	token = base64.RawURLEncoding.EncodeToString(b[:])
	return token, hashToken(token), nil
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

func (s *Service) startSession(ctx context.Context, userID int64) (string, time.Time, error) {
	token, hash, err := newToken()
	if err != nil {
		return "", time.Time{}, err
	}
	now := s.now()
	expires := now.Add(SessionTTL)
	if _, err := s.db.Collection(mdb.Sessions).InsertOne(ctx, sessionDoc{ID: hash, UserID: userID, ExpiresAt: expires, CreatedAt: now}); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// Signup creates the user and a first session.
func (s *Service) Signup(ctx context.Context, email, password string) (User, string, time.Time, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return User{}, "", time.Time{}, err
	}
	if err := checkPassword(password); err != nil {
		return User{}, "", time.Time{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, "", time.Time{}, err
	}
	id, err := mdb.NextID(ctx, s.db, mdb.Users)
	if err != nil {
		return User{}, "", time.Time{}, err
	}
	doc := userDoc{ID: id, Email: email, PasswordHash: hash, CreatedAt: s.now()}
	if _, err := s.db.Collection(mdb.Users).InsertOne(ctx, doc); err != nil {
		if mdb.IsDup(err) { // the unique index on email decides, so two sign-ups cannot both win
			return User{}, "", time.Time{}, ErrEmailTaken
		}
		return User{}, "", time.Time{}, err
	}
	token, expires, err := s.startSession(ctx, id)
	return doc.user(), token, expires, err
}

// Login checks credentials and starts a session. Unknown email and wrong password
// return the same error and take about the same time.
func (s *Service) Login(ctx context.Context, email, password string) (User, string, time.Time, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var d userDoc
	err := s.db.Collection(mdb.Users).FindOne(ctx, bson.D{{Key: "email", Value: email}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		burnPassword(password)
		return User{}, "", time.Time{}, ErrInvalidCredentials
	}
	if err != nil {
		return User{}, "", time.Time{}, err
	}
	ok, verr := VerifyPassword(password, d.PasswordHash)
	if verr != nil || !ok {
		return User{}, "", time.Time{}, ErrInvalidCredentials
	}
	// Drop this user's expired sessions while we are here.
	s.db.Collection(mdb.Sessions).DeleteMany(ctx, bson.D{{Key: "user_id", Value: d.ID}, {Key: "expires_at", Value: bson.D{{Key: "$lte", Value: s.now()}}}})
	token, expires, err := s.startSession(ctx, d.ID)
	return d.user(), token, expires, err
}

// Logout deletes the session for token. Unknown tokens are not an error.
func (s *Service) Logout(ctx context.Context, token string) error {
	_, err := s.db.Collection(mdb.Sessions).DeleteOne(ctx, bson.D{{Key: "_id", Value: hashToken(token)}})
	return err
}

// Authenticate resolves a session token to its user.
func (s *Service) Authenticate(ctx context.Context, token string) (User, error) {
	// The expiry is checked here as well as by the TTL index, which only sweeps every minute.
	var sess sessionDoc
	err := s.db.Collection(mdb.Sessions).FindOne(ctx, bson.D{
		{Key: "_id", Value: hashToken(token)}, {Key: "expires_at", Value: bson.D{{Key: "$gt", Value: s.now()}}},
	}).Decode(&sess)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return User{}, ErrNoSession
	}
	if err != nil {
		return User{}, err
	}
	var d userDoc
	err = s.db.Collection(mdb.Users).FindOne(ctx, bson.D{{Key: "_id", Value: sess.UserID}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return User{}, ErrNoSession // the user was deleted: the session is worthless
	}
	return d.user(), err
}
