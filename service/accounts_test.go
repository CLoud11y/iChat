package service

import (
	"context"
	"encoding/json"
	"errors"
	"iChat/config"
	"iChat/database"
	"iChat/models"
	"iChat/utils"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type accountStub struct {
	user                  *models.User
	err                   error
	ctx                   context.Context
	phone, name, password string
	reads, writes, online int
}

func (s *accountStub) GetUserByPhone(ctx context.Context, phone string) (*models.User, error) {
	s.ctx, s.phone = ctx, phone
	s.reads++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.user, s.err
}
func (s *accountStub) Signup(ctx context.Context, phone, name, password string) (*models.User, error) {
	s.ctx, s.phone, s.name, s.password = ctx, phone, name, password
	s.writes++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.user, s.err
}
func (s *accountStub) Online(user *models.User) { s.online++ }
func (s *accountStub) Offline(uint)             {}

func accountTokens(t *testing.T) *utils.TokenService {
	t.Helper()
	var conf config.Config
	conf.JWT.Key, conf.JWT.TokenHourLifeSpan = "account-test-key", 1
	tokens, err := utils.NewTokenService(conf, &authBlacklist{})
	if err != nil {
		t.Fatal(err)
	}
	return tokens
}

func accountRequest(handler gin.HandlerFunc, body string, ctx context.Context) *httptest.ResponseRecorder {
	r := gin.New()
	r.POST("/", handler)
	req := httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func assertAccountUnavailable(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	var body struct {
		Code int `json:"code"`
		Data struct {
			Reason string `json:"reasonCode"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusServiceUnavailable || body.Code != -1 || body.Data.Reason != "DEPENDENCY_UNAVAILABLE" || strings.Contains(response.Body.String(), "private SQL") {
		t.Fatalf("incorrect storage error response: status=%d body=%s", response.Code, response.Body)
	}
}

func TestAccountStorageFailures(t *testing.T) {
	for _, action := range []string{"login", "register"} {
		for _, failure := range []string{"nil_store", "nil_db", "query_error", "nil_user", "zero_id", "canceled"} {
			t.Run(action+"/"+failure, func(t *testing.T) {
				stub := &accountStub{user: &models.User{Model: gorm.Model{ID: 7}, Password: utils.Encrypt("pw")}}
				var users AccountUsers = stub
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				switch failure {
				case "nil_store":
					users = nil
				case "nil_db":
					users = database.NewUserManager(nil)
				case "query_error":
					stub.err = errors.New("private SQL connection details")
				case "nil_user":
					stub.user = nil
				case "zero_id":
					stub.user.ID = 0
				case "canceled":
					cancel()
				}
				handler := LoginUser(users, accountTokens(t))
				body := `{"account":"phone","password":"pw"}`
				if action == "register" {
					handler = RegisterUser(users)
					body = `{"phone":"phone","name":"Alice","password":"pw","confirmPsw":"pw"}`
				}
				response := accountRequest(handler, body, ctx)
				assertAccountUnavailable(t, response)
				if stub.online != 0 {
					t.Fatal("failed account request marked user online")
				}
				if failure == "canceled" && stub.ctx != ctx {
					t.Fatal("request context was not forwarded")
				}
			})
		}
	}
}

func TestAccountBusinessErrorsAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		register         bool
		err              error
		calls            int
	}{
		{name: "absent_user", body: `{"account":"phone","password":"pw"}`, want: "account or password is incorrect", err: database.ErrUserNotFound, calls: 1},
		{name: "wrong_password", body: `{"account":"phone","password":"wrong"}`, want: "account or password is incorrect", calls: 1},
		{name: "duplicate_phone", register: true, body: `{"phone":"phone","password":"pw","confirmPsw":"pw"}`, want: "phone is already registered", err: database.ErrPhoneRegistered, calls: 1},
		{name: "password_mismatch", register: true, body: `{"password":"pw","confirmPsw":"other"}`, want: "two passwords don't match"},
		{name: "invalid_login_json", body: `{`, want: "invalid login request"},
		{name: "invalid_register_json", register: true, body: `{`, want: "invalid register request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &accountStub{user: &models.User{Model: gorm.Model{ID: 7}, Password: utils.Encrypt("pw")}, err: tc.err}
			handler := LoginUser(stub, accountTokens(t))
			if tc.register {
				handler = RegisterUser(stub)
			}
			response := accountRequest(handler, tc.body, context.Background())
			var body struct {
				Code int    `json:"code"`
				Msg  string `json:"msg"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusOK || body.Code != -1 || body.Msg != tc.want || stub.online != 0 || stub.reads+stub.writes != tc.calls {
				t.Fatalf("incorrect business response: status=%d body=%s calls=%d", response.Code, response.Body, stub.reads+stub.writes)
			}
		})
	}
}

func TestAccountSuccessUsesInjectedRepository(t *testing.T) {
	// Poison the legacy global: handlers must use the supplied repository.
	previous := database.Umanager
	database.Umanager = database.NewUserManager(nil)
	t.Cleanup(func() { database.Umanager = previous })
	stub := &accountStub{user: &models.User{Model: gorm.Model{ID: 7}, Name: "Alice", Password: utils.Encrypt("pw"), Phone: "phone"}}
	tokens := accountTokens(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response := accountRequest(LoginUser(stub, tokens), `{"account":"phone","password":"pw"}`, ctx)
	var login struct {
		Code int       `json:"code"`
		Data LoginResp `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || login.Code != 0 || stub.online != 1 || stub.ctx != ctx || stub.phone != "phone" || login.Data.UserInfo.ID != 7 || login.Data.Token == "" || strings.Contains(response.Body.String(), stub.user.Password) {
		t.Fatalf("incorrect login: %s", response.Body)
	}
	identity, err := tokens.ValidateToken(ctx, login.Data.Token)
	if err != nil || identity.UserID != 7 {
		t.Fatalf("issued token identity=%+v err=%v", identity, err)
	}
	response = accountRequest(RegisterUser(stub), `{"phone":"new-phone","name":"Bob","password":"pw","confirmPsw":"pw"}`, ctx)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"code":0`) || stub.writes != 1 || stub.ctx != ctx || stub.phone != "new-phone" || stub.name != "Bob" || !utils.VerifyPassword("pw", stub.password) || stub.password == "pw" {
		t.Fatalf("incorrect signup: %s", response.Body)
	}
}

func TestLoginTokenFailureDoesNotMarkOnline(t *testing.T) {
	stub := &accountStub{user: &models.User{Model: gorm.Model{ID: 7}, Password: utils.Encrypt("pw")}}
	response := accountRequest(LoginUser(stub, nil), `{"account":"phone","password":"pw"}`, context.Background())
	assertAccountUnavailable(t, response)
	if stub.online != 0 {
		t.Fatal("token failure marked user online")
	}
}
