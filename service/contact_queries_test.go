package service

import (
	"context"
	"encoding/json"
	"errors"
	"iChat/database"
	"iChat/models"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type contactStub struct {
	friends, members                     []models.User
	groups                               []models.Group
	friendErr, groupErr, memberErr       error
	friendCalls, groupCalls, memberCalls int
	ctx                                  context.Context
	id                                   uint
}

func (s *contactStub) SearchFriends2(ctx context.Context, uid uint) ([]models.User, error) {
	s.friendCalls++
	s.ctx, s.id = ctx, uid
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.friends, s.friendErr
}
func (s *contactStub) GetGroupsByUid2(ctx context.Context, uid uint) ([]models.Group, error) {
	s.groupCalls++
	s.ctx, s.id = ctx, uid
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.groups, s.groupErr
}
func (s *contactStub) GetGroupUsers(ctx context.Context, gid uint) ([]models.User, error) {
	s.memberCalls++
	s.ctx, s.id = ctx, gid
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.members, s.memberErr
}
func contactHandler(action string, queries ContactQueries) gin.HandlerFunc {
	switch action {
	case "contacts":
		return GetContacts(queries)
	case "friends":
		return SearchFriends(queries.Friends)
	case "groups":
		return LoadGroups(queries.Groups)
	default:
		return LoadGroupUsers(queries.Groups)
	}
}
func contactRequest(handler gin.HandlerFunc, body string, ctx context.Context, uid uint) *httptest.ResponseRecorder {
	return accountRequest(func(c *gin.Context) { c.Set("uid", uid); handler(c) }, body, ctx)
}
func contactData(t *testing.T, response *httptest.ResponseRecorder, target interface{}) int {
	t.Helper()
	var body struct {
		Code  int             `json:"code"`
		Data  json.RawMessage `json:"data"`
		Total int             `json:"total"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || body.Code != 0 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if err := json.Unmarshal(body.Data, target); err != nil {
		t.Fatal(err)
	}
	return body.Total
}

func TestContactQueryFailures(t *testing.T) {
	for _, action := range []string{"contacts", "friends", "groups", "members"} {
		for _, failure := range []string{"missing", "nil_db", "query_error", "canceled"} {
			t.Run(action+"/"+failure, func(t *testing.T) {
				stub := &contactStub{}
				queries := ContactQueries{Friends: stub, Groups: stub}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				switch failure {
				case "missing":
					queries = ContactQueries{}
				case "nil_db":
					queries = ContactQueries{Friends: database.NewRelationManager(nil), Groups: database.NewGroupManager(nil)}
				case "query_error":
					stub.friendErr = errors.New("private SQL failure")
					stub.groupErr, stub.memberErr = stub.friendErr, stub.friendErr
				case "canceled":
					cancel()
				}
				response := contactRequest(contactHandler(action, queries), `{"group_id":9,"uid":999}`, ctx, 42)
				assertAccountUnavailable(t, response)
				if failure == "canceled" && stub.ctx != ctx {
					t.Fatal("request context was not forwarded")
				}
			})
		}
	}
}

func TestContactsNeverReturnPartialSuccess(t *testing.T) {
	for _, failure := range []string{"friends", "groups", "missing_groups"} {
		t.Run(failure, func(t *testing.T) {
			stub := &contactStub{friends: []models.User{{Model: gorm.Model{ID: 7}, Name: "must-not-leak-partial-friend"}}}
			queries := ContactQueries{Friends: stub, Groups: stub}
			switch failure {
			case "friends":
				stub.friendErr = errors.New("private SQL failure")
			case "groups":
				stub.groupErr = errors.New("private SQL failure")
			case "missing_groups":
				queries.Groups = nil
			}
			response := contactRequest(GetContacts(queries), `{}`, context.Background(), 42)
			assertAccountUnavailable(t, response)
			if strings.Contains(response.Body.String(), "must-not-leak-partial-friend") {
				t.Fatal("partial contacts were returned on failure")
			}
			if failure == "friends" && stub.groupCalls != 0 {
				t.Fatal("group query ran after friend query failed")
			}
			if failure == "groups" && (stub.friendCalls != 1 || stub.groupCalls != 1) {
				t.Fatal("second query failure was not exercised")
			}
		})
	}
}

func TestContactQuerySuccessAndEmptyResults(t *testing.T) {
	oldFriends, oldGroups := database.Rmanager, database.Gmanager
	database.Rmanager, database.Gmanager = nil, nil
	t.Cleanup(func() { database.Rmanager, database.Gmanager = oldFriends, oldGroups })
	for _, empty := range []bool{false, true} {
		stub := &contactStub{friends: []models.User{}, members: []models.User{}, groups: []models.Group{}}
		if !empty {
			stub.friends = []models.User{{Model: gorm.Model{ID: 7}, Name: "Alice", Phone: "phone", Password: "secret"}}
			stub.members = stub.friends
			stub.groups = []models.Group{{Model: gorm.Model{ID: 9}, Name: "Study", OwnerId: 42}}
		}
		ctx, cancel := context.WithCancel(context.Background())
		queries := ContactQueries{Friends: stub, Groups: stub}
		for _, action := range []string{"contacts", "friends", "groups", "members"} {
			response := contactRequest(contactHandler(action, queries), `{"group_id":9,"uid":999}`, ctx, 42)
			wantID := uint(42)
			if action == "members" {
				wantID = 9
			}
			if stub.ctx != ctx || stub.id != wantID {
				t.Fatal("query received incorrect context or identity")
			}
			if strings.Contains(response.Body.String(), "secret") {
				t.Fatal("password leaked in response")
			}
			var raw []json.RawMessage
			total := contactData(t, response, &raw)
			wantCount := 1
			if action == "contacts" {
				wantCount = 2
			}
			if empty {
				wantCount = 0
			}
			if len(raw) != wantCount || (action != "contacts" && total != wantCount) {
				t.Fatalf("action=%s count=%d total=%d", action, len(raw), total)
			}
			if !empty {
				switch action {
				case "contacts":
					var data []ContactInfo
					contactData(t, response, &data)
					if data[0].Id != 7 || data[0].DisplayName != "Alice" || data[0].IsGroup != 0 || data[1].Id != 9 || data[1].IsGroup != 1 {
						t.Fatal("contact mapping changed")
					}
				case "friends":
					var data []models.User
					contactData(t, response, &data)
					if data[0].ID != 7 || data[0].Phone != "phone" {
						t.Fatal("friend mapping changed")
					}
				case "groups":
					var data []models.Group
					contactData(t, response, &data)
					if data[0].ID != 9 || data[0].Name != "Study" {
						t.Fatal("group mapping changed")
					}
				case "members":
					var data []struct {
						UserID   uint        `json:"user_id"`
						UserInfo ContactInfo `json:"userInfo"`
					}
					contactData(t, response, &data)
					if data[0].UserID != 7 || data[0].UserInfo.DisplayName != "Alice" {
						t.Fatal("member mapping changed")
					}
				}
			}
		}
		cancel()
	}
}

func TestGroupUsersInvalidRequestDoesNotQuery(t *testing.T) {
	stub := &contactStub{}
	response := contactRequest(LoadGroupUsers(stub), `{`, context.Background(), 42)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"code":-1`) || stub.memberCalls != 0 {
		t.Fatalf("invalid request: %s", response.Body)
	}
}
