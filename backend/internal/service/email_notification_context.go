package service

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"time"
)

// Notification-only adapter: other transactional email APIs retain their
// existing behavior. Cancellation closes the active SMTP connection, including
// while waiting for its greeting, TLS handshake, DATA or QUIT.
func (s *EmailService) SendNotificationEmail(ctx context.Context, to, subject, body string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	config, err := s.GetSMTPConfig(ctx)
	if err != nil {
		return err
	}
	message, err := buildSMTPMessage(config, to, subject, body)
	if err != nil {
		return err
	}
	client, closeClient, err := connectNotificationSMTP(ctx, config)
	if err != nil {
		return err
	}
	defer closeClient()
	if err = client.Auth(smtp.PlainAuth("", config.Username, config.Password, config.Host)); err != nil {
		return fmt.Errorf("smtp auth: %w", err)
	}
	if err = client.Mail(message.envelopeFrom); err != nil {
		return fmt.Errorf("smtp mail: %w", err)
	}
	if err = client.Rcpt(message.envelopeTo); err != nil {
		return fmt.Errorf("smtp rcpt: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err = w.Write(message.data); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	_ = client.Quit()
	return ctx.Err()
}

func connectNotificationSMTP(ctx context.Context, config *SMTPConfig) (*smtp.Client, func(), error) {
	if config == nil {
		return nil, nil, errors.New("missing SMTP configuration")
	}
	addr := net.JoinHostPort(config.Host, strconv.Itoa(config.Port))
	connect := func(implicit bool) (*smtp.Client, func(), error) {
		dialer := &net.Dialer{Timeout: smtpDialTimeout}
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, nil, err
		}
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		cleanup := func() { stop(); _ = conn.Close() }
		deadline := time.Now().Add(smtpIOTimeout)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		if err = conn.SetDeadline(deadline); err != nil {
			cleanup()
			return nil, nil, err
		}
		smtpConn := conn
		if implicit {
			secured := tls.Client(conn, smtpTLSConfig(config.Host))
			if err = secured.HandshakeContext(ctx); err != nil {
				cleanup()
				return nil, nil, err
			}
			smtpConn = secured
		}
		client, err := smtp.NewClient(smtpConn, config.Host)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		if !implicit {
			if supported, _ := client.Extension("STARTTLS"); supported {
				if err = client.StartTLS(smtpTLSConfig(config.Host)); err != nil {
					cleanup()
					return nil, nil, err
				}
			} else if config.UseTLS {
				cleanup()
				return nil, nil, errors.New("smtp server does not support STARTTLS")
			}
		}
		return client, cleanup, nil
	}
	if config.UseTLS {
		client, cleanup, err := connect(true)
		var recordErr tls.RecordHeaderError
		if err == nil || !errors.As(err, &recordErr) || ctx.Err() != nil {
			return client, cleanup, err
		}
	}
	return connect(false)
}
