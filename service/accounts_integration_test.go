//go:build integration

package service

import (
	"context"
	"encoding/json"
	"errors"
	"iChat/database"
	"iChat/models"
	"iChat/tests/testenv"
	"iChat/utils"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestAccountMySQLIntegration(t *testing.T) {
	f := testenv.NewLegacyFixture(t)
	users := database.NewUserManager(f.DB)
	tokens := accountTokens(t)
	ctx := context.Background()
	response := accountRequest(RegisterUser(users), `{"phone":"account-fixture","name":"Alice","password":"pw","confirmPsw":"pw"}`, ctx)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"code":0`) {
		t.Fatalf("signup failed: %s", response.Body)
	}
	saved, err := users.GetUserByPhone(ctx, "account-fixture")
	if err != nil || saved.ID == 0 || saved.Name != "Alice" || saved.Identifier != "account-fixture" || !utils.VerifyPassword("pw", saved.Password) {
		t.Fatalf("saved user invalid: err=%v", err)
	}
	response = accountRequest(LoginUser(users, tokens), `{"account":"account-fixture","password":"pw"}`, ctx)
	var login struct {
		Code int       `json:"code"`
		Data LoginResp `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || login.Code != 0 || login.Data.UserInfo.ID != saved.ID {
		t.Fatalf("login failed: %s", response.Body)
	}
	if identity, err := tokens.ValidateToken(ctx, login.Data.Token); err != nil || identity.UserID != saved.ID {
		t.Fatalf("token invalid: %v", err)
	}
	if _, err := users.GetUserByPhone(ctx, "absent-fixture"); !errors.Is(err, database.ErrUserNotFound) {
		t.Fatalf("missing user error=%v", err)
	}

	// Competing signups must converge on the unique constraint without
	// overwriting a winner or leaking a raw duplicate-key error to callers.
	const attempts = 8
	start := make(chan struct{})
	results := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := users.Signup(ctx, "concurrent-fixture", "Concurrent", utils.Encrypt("pw"))
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	success, duplicate := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, database.ErrPhoneRegistered):
			duplicate++
		default:
			t.Errorf("unexpected signup error: %v", err)
		}
	}
	if success != 1 || duplicate != attempts-1 {
		t.Fatalf("signup outcomes: success=%d duplicate=%d", success, duplicate)
	}
	var count int64
	if err := f.DB.Model(&models.User{}).Where("phone = ?", "concurrent-fixture").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("concurrent signup count=%d err=%v", count, err)
	}

	// In this run's isolated schema, let the lookup succeed but make the
	// database reject the insert. This verifies the write-error path separately.
	if err := f.DB.Exec("ALTER TABLE `user` ADD CONSTRAINT account_test_reject CHECK (phone <> 'blocked-signup')").Error; err != nil {
		t.Fatal(err)
	}
	response = accountRequest(RegisterUser(users), `{"phone":"blocked-signup","name":"Blocked","password":"pw","confirmPsw":"pw"}`, ctx)
	assertAccountUnavailable(t, response)
	if err := f.DB.Model(&models.User{}).Where("phone = ?", "blocked-signup").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("failed write left a user: count=%d err=%v", count, err)
	}
	if err := f.DB.Exec("ALTER TABLE `user` DROP CHECK account_test_reject").Error; err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := users.Signup(canceled, "canceled-signup", "Canceled", utils.Encrypt("pw")); !errors.Is(err, context.Canceled) || !errors.Is(err, database.ErrStorageUnavailable) {
		t.Fatalf("canceled signup error=%v", err)
	}
	if _, err := users.GetUserByPhone(canceled, saved.Phone); !errors.Is(err, context.Canceled) || !errors.Is(err, database.ErrStorageUnavailable) {
		t.Fatalf("canceled lookup error=%v", err)
	}
	if err := f.DB.Model(&models.User{}).Where("phone = ?", "canceled-signup").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("canceled signup wrote a row: count=%d err=%v", count, err)
	}
	pool, err := f.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	assertAccountUnavailable(t, accountRequest(LoginUser(users, tokens), `{"account":"account-fixture","password":"pw"}`, ctx))
	assertAccountUnavailable(t, accountRequest(RegisterUser(users), `{"phone":"closed-signup","name":"Closed","password":"pw","confirmPsw":"pw"}`, ctx))
}
