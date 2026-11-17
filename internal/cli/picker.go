package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/agy"
	"github.com/lifan-builds/quotaswitch/internal/registry"
	"golang.org/x/term"
)

var errCancelled = errors.New("cancelled")

func (a *App) chooseAccount(accounts []registry.Account, activeID, title string, numbers map[string]int, withUsage bool) (*registry.Account, error) {
	if len(accounts) == 0 {
		return nil, errors.New("no accounts available")
	}
	if !a.TTY || a.StdinFile == nil {
		return a.chooseAccountNumberedMode(accounts, activeID, title, numbers, withUsage)
	}
	return a.chooseAccountInteractiveMode(accounts, activeID, title, numbers, withUsage)
}

func matchRowNumber(rows []accountRow, value string) int {
	target := strings.TrimLeft(value, "0")
	if target == "" {
		return -1
	}
	for i := range rows {
		if strconv.Itoa(rows[i].Number) == target {
			return i
		}
	}
	for i := range rows {
		if strings.HasPrefix(strconv.Itoa(rows[i].Number), target) {
			return i
		}
	}
	return -1
}

func accountNumbers(reg *registry.Registry) map[string]int {
	numbers := make(map[string]int, len(reg.Accounts))
	for i, account := range reg.Accounts {
		numbers[account.ID] = i + 1
	}
	return numbers
}

func (a *App) pickerRows(accounts []registry.Account, activeID string, numbers map[string]int) []accountRow {
	now := a.now()
	rows := make([]accountRow, len(accounts))
	for i, account := range accounts {
		number := i + 1
		if numbers != nil {
			if value, ok := numbers[account.ID]; ok {
				number = value
			}
		}
		row := accountRow{
			Number:     number,
			ID:         account.ID,
			Active:     account.ID == activeID,
			Stored:     true,
			Email:      account.Email,
			Alias:      account.Alias,
			AuthMethod: account.AuthMethod,
			AddedAt:    timePtr(account.AddedAt),
			LastUsedAt: timePtr(account.LastUsedAt),
		}
		raw, err := a.Store.LoadToken(account.ID)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				row.UsageError = "corrupt_snapshot"
				if !a.JSON {
					a.warn("could not read stored snapshot for %s: %v", account.DisplayName(), err)
				}
			}
		} else if tf, err := agy.ParseTokenFile(raw); err != nil {
			row.UsageError = "corrupt_snapshot"
			if !a.JSON {
				a.warn("stored snapshot for %s is invalid: %v", account.DisplayName(), err)
			}
		} else {
			row.token = tf
			if expiry, ok := tf.ExpiryTime(); ok {
				value := expiry
				row.ExpiresAt = &value
				row.Expired = !expiry.After(now)
			}
			if row.Email == "" {
				row.Email = tf.Email()
			}
			if row.AuthMethod == "" {
				row.AuthMethod = tf.AuthMethod
			}
		}
		rows[i] = row
	}
	return rows
}

func (a *App) chooseAccountNumberedMode(accounts []registry.Account, activeID, title string, numbers map[string]int, withUsage bool) (*registry.Account, error) {
	fmt.Fprintln(a.Out, title)
	rows := a.pickerRows(accounts, activeID, numbers)
	if withUsage {
		fmt.Fprintln(a.Out, "Loading quota...")
		a.enrichUsage(rows)
	}
	a.renderAccountRows(rows, a.now())
	answer, err := a.readLine("Select account number: ")
	if err != nil {
		return nil, err
	}
	if answer == "" && len(accounts) == 1 {
		answer = strconv.Itoa(rows[0].Number)
	}
	number, err := strconv.Atoi(answer)
	if err == nil {
		for i, row := range rows {
			if row.Number == number {
				account := accounts[i]
				return &account, nil
			}
		}
	}
	return nil, fmt.Errorf("invalid selection %q", answer)
}

func (a *App) chooseAccountInteractiveMode(accounts []registry.Account, activeID, title string, numbers map[string]int, withUsage bool) (*registry.Account, error) {
	fd := int(a.StdinFile.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		fmt.Fprintf(a.Err, "warning: interactive terminal unavailable (%v); using numbered selection\n", err)
		return a.chooseAccountNumberedMode(accounts, activeID, title, numbers, withUsage)
	}
	defer term.Restore(fd, state)

	fmt.Fprint(a.Out, "\x1b[?1049h\x1b[?25l")
	defer fmt.Fprint(a.Out, "\x1b[?25h\x1b[?1049l")

	index := 0
	for i, account := range accounts {
		if account.ID == activeID {
			index = i
			break
		}
	}
	rows := a.pickerRows(accounts, activeID, numbers)
	if withUsage {
		fmt.Fprintf(a.Out, "%s\r\n", a.bold(title))
		fmt.Fprintf(a.Out, "%s\r\n", a.dim("Loading quota..."))
		a.enrichUsage(rows)
	}
	now := a.now()

	render := func() {
		fmt.Fprint(a.Out, "\x1b[H\x1b[2J")
		fmt.Fprintf(a.Out, "%s\r\n", a.bold(title))
		fmt.Fprintf(a.Out, "%s\r\n", a.dim("up/down or j/k move · 0-9 jump · enter select · q cancel"))
		fmt.Fprint(a.Out, "\r\n")
		a.renderAccountTable(rows, now, index, "\r\n")
	}

	render()
	keys := make(chan keyEvent, 8)
	go readKeyEvents(a.StdinFile, keys)
	digits := ""
	var digitAt time.Time
	for key := range keys {
		now := time.Now()
		if digit, ok := key.digit(); ok {
			if !digitAt.IsZero() && now.Sub(digitAt) > typeaheadTimeout {
				digits = ""
			}
			digits += string(digit)
			digitAt = now
			if matched := matchRowNumber(rows, digits); matched >= 0 {
				index = matched
			}
			render()
			continue
		}
		digits = ""
		digitAt = time.Time{}
		switch key.kind {
		case keyCtrlC, keyEsc:
			return nil, errCancelled
		case keyEnter:
			account := accounts[index]
			return &account, nil
		case keyUp:
			if index > 0 {
				index--
			}
		case keyDown:
			if index < len(accounts)-1 {
				index++
			}
		case keyHome:
			index = 0
		case keyEnd:
			index = len(accounts) - 1
		case keyRune:
			switch key.r {
			case 'q':
				return nil, errCancelled
			case 'k':
				if index > 0 {
					index--
				}
			case 'j':
				if index < len(accounts)-1 {
					index++
				}
			case 'g':
				index = 0
			case 'G':
				index = len(accounts) - 1
			default:
				continue
			}
		default:
			continue
		}
		render()
	}
	return nil, errors.New("input closed")
}

func selectionSummary(accounts []registry.Account) string {
	names := make([]string, 0, len(accounts))
	for _, account := range accounts {
		names = append(names, account.DisplayName())
	}
	return strings.Join(names, ", ")
}
