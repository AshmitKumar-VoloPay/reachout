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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
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
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, now: time.Now}
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
	expires := s.now().Add(SessionTTL)
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		hash, userID, expires); err != nil {
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
	var u User
	err = s.pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING id, email, created_at`,
		email, hash).Scan(&u.ID, &u.Email, &u.CreatedAt)
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return User{}, "", time.Time{}, ErrEmailTaken
		}
		return User{}, "", time.Time{}, err
	}
	token, expires, err := s.startSession(ctx, u.ID)
	return u, token, expires, err
}

// Login checks credentials and starts a session. Unknown email and wrong password
// return the same error and take about the same time.
func (s *Service) Login(ctx context.Context, email, password string) (User, string, time.Time, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var u User
	var hash string
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, created_at, password_hash FROM users WHERE email = $1`, email).
		Scan(&u.ID, &u.Email, &u.CreatedAt, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		burnPassword(password)
		return User{}, "", time.Time{}, ErrInvalidCredentials
	}
	if err != nil {
		return User{}, "", time.Time{}, err
	}
	ok, verr := VerifyPassword(password, hash)
	if verr != nil || !ok {
		return User{}, "", time.Time{}, ErrInvalidCredentials
	}
	// Drop this user's expired sessions while we are here.
	s.pool.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1 AND expires_at <= $2`, u.ID, s.now())
	token, expires, err := s.startSession(ctx, u.ID)
	return u, token, expires, err
}

// Logout deletes the session for token. Unknown tokens are not an error.
func (s *Service) Logout(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hashToken(token))
	return err
}

// Authenticate resolves a session token to its user.
func (s *Service) Authenticate(ctx context.Context, token string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT u.id, u.email, u.created_at
		   FROM sessions se JOIN users u ON u.id = se.user_id
		  WHERE se.token_hash = $1 AND se.expires_at > $2`,
		hashToken(token), s.now()).Scan(&u.ID, &u.Email, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNoSession
	}
	return u, err
}
