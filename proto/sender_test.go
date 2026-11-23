package proto

import (
	"encoding/json"
	"testing"
)

func TestASendersWireForm(t *testing.T) {
	for _, tc := range []struct {
		sender Sender
		want   string
	}{
		{Sender{Kind: SenderService}, `{"kind":"service"}`},
		{Sender{Kind: SenderSupervisor, Session: 7}, `{"kind":"supervisor","session":7}`},
	} {
		got, err := json.Marshal(tc.sender)
		if err != nil || string(got) != tc.want {
			t.Errorf("Marshal(%+v) = %s, %v; want %s", tc.sender, got, err, tc.want)
		}
	}
}
