package service

import (
	"strings"
	"testing"
)

func TestBuildSMTPMessagePreservesParsedFromName(t *testing.T) {
	message, err := buildSMTPMessage(&SMTPConfig{
		Host: "smtp.example.com",
		From: "Support Team <support@example.com>",
	}, "user@example.net", "测试主题", "<p>hello</p>\n下一行")
	if err != nil {
		t.Fatalf("buildSMTPMessage() error = %v", err)
	}
	text := string(message.data)
	if !strings.Contains(text, "From: \"Support Team\" <support@example.com>\r\n") {
		t.Fatalf("From header lost parsed display name: %q", text)
	}
	if !strings.Contains(text, "Content-Transfer-Encoding: quoted-printable\r\n") {
		t.Fatalf("missing quoted-printable transfer encoding: %q", text)
	}
	if message.envelopeFrom != "support@example.com" || message.envelopeTo != "user@example.net" {
		t.Fatalf("unexpected SMTP envelope: from=%q to=%q", message.envelopeFrom, message.envelopeTo)
	}
}

func TestBuildSMTPMessageRejectsHeaderInjectionAddresses(t *testing.T) {
	_, err := buildSMTPMessage(&SMTPConfig{From: "sender@example.com"}, "victim@example.net\r\nBcc: attacker@example.net", "subject", "body")
	if err == nil {
		t.Fatal("buildSMTPMessage() accepted a recipient containing a line break")
	}
}
