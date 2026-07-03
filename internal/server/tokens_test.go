package server

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestListTokensOwnOnly(t *testing.T) {
	srv, st := testServer(t)
	tokA := seedUserToken(t, st, "tokusera@b.co", "member")
	tokB := seedUserToken(t, st, "tokuserb@b.co", "member")

	respA := doAuthed(t, "GET", srv.URL+"/v1/tokens", tokA, "")
	defer respA.Body.Close()
	if respA.StatusCode != 200 {
		t.Fatalf("list A: want 200, got %d", respA.StatusCode)
	}
	var listA []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(respA.Body).Decode(&listA); err != nil {
		t.Fatal(err)
	}
	if len(listA) != 1 || listA[0].Name != "test" {
		t.Fatalf("list A = %+v", listA)
	}

	respB := doAuthed(t, "GET", srv.URL+"/v1/tokens", tokB, "")
	defer respB.Body.Close()
	var listB []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(respB.Body).Decode(&listB); err != nil {
		t.Fatal(err)
	}
	if len(listB) != 1 || listB[0].Name != "test" {
		t.Fatalf("list B = %+v", listB)
	}
	if listA[0].ID == listB[0].ID {
		t.Fatalf("tokens should differ per user: %d vs %d", listA[0].ID, listB[0].ID)
	}
}

func TestRevokeOwnTokenLogsOut(t *testing.T) {
	srv, st := testServer(t)
	tok := seedUserToken(t, st, "revoke1@b.co", "member")

	listResp := doAuthed(t, "GET", srv.URL+"/v1/tokens", tok, "")
	defer listResp.Body.Close()
	var list []struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 token, got %+v", list)
	}

	del := doAuthed(t, "DELETE", fmt.Sprintf("%s/v1/tokens/%d", srv.URL, list[0].ID), tok, "")
	defer del.Body.Close()
	if del.StatusCode != 204 {
		t.Fatalf("revoke: want 204, got %d", del.StatusCode)
	}

	me := doAuthed(t, "GET", srv.URL+"/v1/me", tok, "")
	defer me.Body.Close()
	if me.StatusCode != 401 {
		t.Fatalf("revoked token should fail auth, got %d", me.StatusCode)
	}
}

func TestRevokeForeignTokenNotFound(t *testing.T) {
	srv, st := testServer(t)
	tokA := seedUserToken(t, st, "foreigna@b.co", "member")
	tokB := seedUserToken(t, st, "foreignb@b.co", "member")

	listResp := doAuthed(t, "GET", srv.URL+"/v1/tokens", tokB, "")
	defer listResp.Body.Close()
	var list []struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 token, got %+v", list)
	}

	del := doAuthed(t, "DELETE", fmt.Sprintf("%s/v1/tokens/%d", srv.URL, list[0].ID), tokA, "")
	defer del.Body.Close()
	if del.StatusCode != 404 {
		t.Fatalf("foreign revoke: want 404, got %d", del.StatusCode)
	}
}
