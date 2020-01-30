package mail

import (
	"github.com/go-gomail/gomail"
	"github.com/rykov/paperboy/config"

	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"sync"
	"time"
)

func LoadAndSendCampaign(ctx context.Context, cfg *config.AConfig, tmplFile, recipientFile string) error {
	// Load up template and recipientswith frontmatter
	c, err := LoadCampaign(cfg, tmplFile, recipientFile)
	if err != nil {
		return err
	}

	return SendCampaign(ctx, cfg, c)
}

func SendCampaign(ctx context.Context, cfg *config.AConfig, c *Campaign) error {
	// Initialize deliverer
	engine := &deliverer{
		tasks:    make(chan *gomail.Message, 10),
		waiter:   &sync.WaitGroup{},
		campaign: c,
	}

	// Capture context cancellation for graceful exit
	if done := ctx.Done(); done != nil {
		go func() {
			<-done
			engine.close()
		}()
	}

	// Rate configuration
	throttle, workers := time.Duration(0), cfg.Workers
	if cfg.SendRate > 0 {
		throttle = time.Duration(1000 / cfg.SendRate)
		throttle = throttle * time.Millisecond
	}

	// Start queueing emails to keep workers from idling
	fmt.Printf("Sending an email every %s via %d workers\n", throttle, workers)
	go func() {
		for i := range c.Recipients {
			m := gomail.NewMessage()
			if err := c.renderMessage(m, i); err != nil {
				fmt.Printf("Could not queue email: %s\n", err)
				engine.close()
				return
			}

			// Gracefully handle exits and make sure we don't
			// try to queue mails into a closed channel
			if engine.stop {
				// TODO: Dump a cursor that can be used to resume a campaign
				fmt.Printf("Stopped queing before %s\n", m.GetHeader("To"))
				break
			} else {
				engine.tasks <- m
				if throttle > 0 {
					time.Sleep(throttle)
				}
			}
		}
		engine.close()
	}()

	// Start delivery workers
	for i := 0; i < workers; i++ {
		if err := engine.startWorker(i); err != nil {
			engine.close()
			return err
		}

		// HACK: Avoid race warning
		engine.stopL.Lock()
		stopped := engine.stop
		engine.stopL.Unlock()
		if stopped {
			break
		}
	}

	// Wait until everything is done
	engine.waiter.Wait()
	return nil
}

type deliverer struct {
	campaign *Campaign
	waiter   *sync.WaitGroup
	tasks    chan *gomail.Message

	stop  bool
	stopL sync.Mutex
}

func (d *deliverer) close() {
	d.stopL.Lock()
	defer d.stopL.Unlock()

	if !d.stop {
		d.stop = true
		close(d.tasks)
	}
}

func (d *deliverer) startWorker(id int) error {
	fmt.Printf("[%d] Starting worker...\n", id)
	d.waiter.Add(1)

	// Dial up the sender
	sender, err := configureSender(d.campaign.Config)
	if err != nil {
		return err
	}

	go func() {
		defer d.waiter.Done()
		defer fmt.Printf("[%d] Stopping worker...\n", id)
		defer sender.Close()
		c := d.campaign

		for {
			m, more := <-d.tasks
			if !more {
				return
			}
			fmt.Printf("[%d] Sending %s to %s\n", id, c.ID, m.GetHeader("To"))
			if err := gomail.Send(sender, m); err != nil {
				fmt.Printf("[%d] Could not send email: %s\n", id, err)
			}
		}
	}()

	return nil
}

func configureSender(cfg *config.AConfig) (sender gomail.SendCloser, err error) {
	// Dial up SMTP or dryRun
	if cfg.DryRun {
		sender = &dryRunSender{Mails: [][]byte{}}
	} else {
		dialer, err := smtpDialer(&cfg.SMTP)
		if err != nil {
			return nil, err
		}
		sender, err = dialer.Dial()
		if err != nil {
			return nil, err
		}
	}

	// DKIM-signing sender, if configuration is present
	if dCfg := cfg.DKIM; len(dCfg) > 0 {
		sender, err = SendCloserWithDKIM(cfg.AppFs, sender, dCfg)
		if err != nil {
			return nil, err
		}
	}

	return sender, nil
}

func smtpDialer(cfg *config.SMTPConfig) (*gomail.Dialer, error) {
	// Dial to SMTP server (with SSL)
	surl, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, err
	}

	// Populate/validate scheme
	if s := surl.Scheme; s == "" {
		surl.Scheme = "smtps"
	} else if s != "smtp" && s != "smtps" {
		return nil, fmt.Errorf("Invalid SMTP URL scheme: %s", s)
	}

	// Authentication from URL
	var user, pass string
	if auth := surl.User; auth != nil {
		pass, _ = auth.Password()
		user = auth.Username()
	}

	// Authentication overrides
	if cfg.User != "" {
		user = cfg.User
	}
	if cfg.Pass != "" {
		pass = cfg.Pass
	}

	// Port
	var port int
	if i, err := strconv.Atoi(surl.Port()); err == nil {
		port = i
	} else if surl.Scheme == "smtp" {
		port = 25
	} else {
		port = 465
	}

	// Initialize the dialer
	d := gomail.NewDialer(surl.Hostname(), port, user, pass)
	d.SSL = (surl.Scheme == "smtps")
	return d, nil
}

// Allow testing deliveries in libraries via dryRun
var LastRunResult *dryRunSender

type dryRunSender struct {
	lock  sync.Mutex
	Mails [][]byte
}

func (s *dryRunSender) Send(from string, to []string, msg io.WriterTo) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	var buf bytes.Buffer
	msg.WriteTo(&buf)

	s.Mails = append(s.Mails, buf.Bytes())
	return nil
}

func (s *dryRunSender) Close() error {
	LastRunResult = s
	return nil
}
