package service

import (
	"context"
	"errors"
	"fmt"
	"iChat/database"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

type contactWriteStub struct {
	err               error
	ctx               context.Context
	uid, gid          uint
	phone, name, desc string
	calls             int
}

func (s *contactWriteStub) record(ctx context.Context, uid uint) error {
	s.calls++
	s.ctx, s.uid = ctx, uid
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.err
}
func (s *contactWriteStub) AddFriendByPhone(ctx context.Context, uid uint, phone string) error {
	s.phone = phone
	return s.record(ctx, uid)
}
func (s *contactWriteStub) CreateGroup(ctx context.Context, name string, uid uint, desc string) error {
	s.name, s.desc = name, desc
	return s.record(ctx, uid)
}
func (s *contactWriteStub) JoinGroup(ctx context.Context, uid, gid uint) error {
	s.gid = gid
	return s.record(ctx, uid)
}
func (s *contactWriteStub) DeleteGroup(ctx context.Context, uid, gid uint) error {
	s.gid = gid
	return s.record(ctx, uid)
}
func contactWriteHandler(action string, writes ContactWrites) gin.HandlerFunc {
	switch action {
	case "friend":
		return AddFriend(writes.Friends)
	case "create":
		return CreateGroup(writes.Groups)
	case "join":
		return JoinGroup(writes.Groups)
	default:
		return DeleteGroup(writes.Groups)
	}
}

const contactWriteBody = `{"uid":999,"ownerId":999,"targetPhone":"phone","groupId":"9","name":"Study","desc":"Description"}`

func TestContactWrites(t *testing.T) {
	oldFriends, oldGroups, oldUsers := database.Rmanager, database.Gmanager, database.Umanager
	database.Rmanager, database.Gmanager, database.Umanager = nil, nil, nil
	t.Cleanup(func() { database.Rmanager, database.Gmanager, database.Umanager = oldFriends, oldGroups, oldUsers })
	for _, action := range []string{"friend", "create", "join", "delete"} {
		for _, scenario := range []string{"success", "missing", "nil_db", "failure", "canceled", "invalid_body"} {
			t.Run(action+"/"+scenario, func(t *testing.T) {
				stub := &contactWriteStub{}
				writes := ContactWrites{Friends: stub, Groups: stub}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				body := contactWriteBody
				switch scenario {
				case "missing":
					writes = ContactWrites{}
				case "nil_db":
					writes = ContactWrites{Friends: database.NewRelationManager(nil), Groups: database.NewGroupManager(nil)}
				case "failure":
					stub.err = errors.New("private SQL failure")
				case "canceled":
					cancel()
				case "invalid_body":
					body = `{`
				}
				response := contactRequest(contactWriteHandler(action, writes), body, ctx, 42)
				switch scenario {
				case "success":
					if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"code":0`) {
						t.Fatalf("success: %s", response.Body)
					}
					if stub.ctx != ctx || stub.uid != 42 || stub.calls != 1 {
						t.Fatal("incorrect context/identity/call count")
					}
					if (action == "friend" && stub.phone != "phone") || (action == "create" && (stub.name != "Study" || stub.desc != "Description")) || ((action == "join" || action == "delete") && stub.gid != 9) {
						t.Fatal("incorrect arguments")
					}
				case "invalid_body":
					if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"code":-1`) || stub.calls != 0 {
						t.Fatalf("invalid body: %s", response.Body)
					}
				default:
					assertAccountUnavailable(t, response)
				}
				if scenario == "canceled" && stub.ctx != ctx {
					t.Fatal("request context not forwarded")
				}
			})
		}
	}
}

func TestContactWriteBusinessErrors(t *testing.T) {
	for _, business := range []error{database.ErrEmptyFriendPhone, database.ErrSelfFriend, database.ErrUserNotFound, database.ErrRelationExists, database.ErrGroupNotOwned, database.ErrGroupNotFound} {
		stub := &contactWriteStub{err: fmt.Errorf("private SQL: %w", business)}
		response := contactRequest(AddFriend(stub), contactWriteBody, context.Background(), 42)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"code":-1`) || strings.Contains(response.Body.String(), "private SQL") {
			t.Fatalf("business=%v response=%s", business, response.Body)
		}
	}
}

func TestContactWritesRejectInvalidGroupIDs(t *testing.T) {
	for _, action := range []string{"join", "delete"} {
		for _, id := range []string{"", "0", "-1", "1.5", "18446744073709551616", "abc"} {
			stub := &contactWriteStub{}
			response := contactRequest(contactWriteHandler(action, ContactWrites{Groups: stub}), fmt.Sprintf(`{"groupId":%q}`, id), context.Background(), 42)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"code":-1`) || stub.calls != 0 {
				t.Fatalf("%s id=%q response=%s", action, id, response.Body)
			}
		}
	}
}
