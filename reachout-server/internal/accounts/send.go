package accounts

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// SendSettings is what the worker needs to decide whether an account may send right now.
// It is not exposed over HTTP.
type SendSettings struct {
	Status     string
	ConnectCap int        // configured daily connection requests
	MessageCap int        // configured daily messages and follow-ups
	RampStart  time.Time  // date the warm-up began (only the date matters)
	NotBefore  *time.Time // no send before this moment (spacing between sends)
}

func (s *Service) SendSettings(ctx context.Context, accountID int64) (SendSettings, error) {
	var d accountDoc
	err := s.coll().FindOne(ctx, bson.D{eq("_id", accountID)}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return SendSettings{}, ErrNotFound
	}
	if err != nil {
		return SendSettings{}, err
	}
	return SendSettings{
		Status: d.Status, ConnectCap: d.Schedule.DailyConnectCap, MessageCap: d.Schedule.DailyMessageCap,
		RampStart: d.Schedule.RampStartedAt, NotBefore: d.Schedule.NextSendNotBefore,
	}, nil
}

// SetNotBefore records the earliest time the account may send again.
func (s *Service) SetNotBefore(ctx context.Context, accountID int64, t time.Time) error {
	_, err := s.coll().UpdateOne(ctx, bson.D{eq("_id", accountID)},
		bson.D{{Key: "$set", Value: bson.D{eq("schedule.next_send_not_before", t)}}})
	return err
}

// MarkNeedsAttention flags a connected account the worker found unusable (signed out of
// LinkedIn, unsupported language). Sending stops until a refresh or reconnect clears it.
func (s *Service) MarkNeedsAttention(ctx context.Context, accountID int64, status, detail string) error {
	_, err := s.coll().UpdateOne(ctx, bson.D{eq("_id", accountID), eq("status", StatusConnected)},
		bson.D{{Key: "$set", Value: bson.D{eq("status", status), eq("status_detail", detail)}}})
	return err
}
