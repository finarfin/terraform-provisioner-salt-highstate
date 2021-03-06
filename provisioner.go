package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	salt "github.com/finarfin/go-salt-netapi-client/cherrypy"
	"github.com/hashicorp/terraform/helper/schema"
	"github.com/hashicorp/terraform/terraform"
)

// ErrorHighstateFailed indicates that highstate command failed on the minion
var ErrorHighstateFailed = errors.New("Highstate execution failed")

// ErrorMinionNotAvailable indicates that the minion was avaliable (offline or not registered)
var ErrorMinionNotAvailable = errors.New("Minion is not available")

/*
Provisioner is waits for a minion to connect to a master;
and applies highstate then reports whether it completed successfully.
*/
func Provisioner() terraform.ResourceProvisioner {
	return &schema.Provisioner{
		Schema: map[string]*schema.Schema{
			"address": {
				Type:     schema.TypeString,
				Required: true,
			},
			"username": {
				Type:     schema.TypeString,
				Required: true,
			},
			"password": {
				Type:     schema.TypeString,
				Required: true,
			},
			"backend": {
				Type:     schema.TypeString,
				Required: true,
			},
			"skip_verify": {
				Type:     schema.TypeBool,
				Optional: true,
				Default:  false,
			},
			"minion_id": {
				Type:     schema.TypeString,
				Required: true,
			},
			"timeout_minutes": {
				Type:     schema.TypeInt,
				Optional: true,
				Default:  30,
			},
			"interval_secs": {
				Type:     schema.TypeInt,
				Optional: true,
				Default:  10,
			},
		},

		ApplyFunc: apply,
	}
}

func apply(ctx context.Context) error {
	o := ctx.Value(schema.ProvOutputKey).(terraform.UIOutput)
	d := ctx.Value(schema.ProvConfigDataKey).(*schema.ResourceData)

	cli := salt.NewClient(
		d.Get("address").(string),
		d.Get("username").(string),
		d.Get("password").(string),
		d.Get("backend").(string),
		d.Get("skip_verify").(bool),
	)

	if err := cli.Login(ctx); err != nil {
		return err
	}

	timeout := time.Duration(d.Get("timeout_minutes").(int)) * time.Minute
	interval := time.Duration(d.Get("interval_secs").(int)) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	minion := d.Get("minion_id").(string)
	attempts := 0
	maxAttempts := 3

	for {
		if attempts >= maxAttempts {
			return fmt.Errorf("%w: could not execute highstate in %d attempts", ErrorMinionNotAvailable, attempts)
		}

		attempts++

		o.Output(fmt.Sprintf("Waiting for minion %s to register with master", minion))
		if err := waitForMinion(ctx, o, cli, minion, interval, timeout); err != nil {
			return err
		}

		o.Output(fmt.Sprintf("Executing state.highstate on minion %s", minion))
		res, err := cli.RunCommand(ctx, salt.Command{
			Client:   salt.LocalClient,
			Target:   salt.ListTarget{Targets: []string{minion}},
			Function: "state.highstate",
		})

		if err != nil {
			return err
		}

		rd, ok := res.(map[string]interface{})
		if !ok {
			return fmt.Errorf("invalid response from Salt Master: expected map; received %s", res)
		}

		md, ok := rd[minion]
		if !ok {
			o.Output(fmt.Sprintf("Minion %s is not registered. Will retry.", minion))
			continue
		}

		data, ok := md.(map[string]interface{})
		if !ok {
			if _, ok := md.(bool); ok {
				o.Output(fmt.Sprintf("Minion %s is not available. Will retry.", minion))
				continue
			}

			return fmt.Errorf("invalid highstate response: expected map; received: %s", data)
		}

		if data["retcode"].(float64) != 0 {
			ls, ok := data["ret"].(map[string]interface{})
			if ok {
				var fails []string
				for k, v := range ls {
					state := v.(map[string]interface{})
					if !state["result"].(bool) {
						fails = append(fails, fmt.Sprintf("State %s failed on %s: %s", k, minion, state["comment"]))
					}
				}

				return fmt.Errorf("%w:\n%s", ErrorHighstateFailed, strings.Join(fails, "\n"))
			}

			return fmt.Errorf("Highstate execution failed on %s: %s", minion, data["ret"])
		}

		return nil
	}
}

func waitForMinion(ctx context.Context, o terraform.UIOutput, cli *salt.Client, minion string, interval time.Duration, timeout time.Duration) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			minionData, err := cli.Minion(ctx, minion)
			if err != nil && !errors.Is(err, salt.ErrorMinionNotFound) {
				return err
			} else if minionData == nil || minionData.Grains == nil {
				time.Sleep(interval)
				continue
			}
		}

		break
	}

	return nil
}
