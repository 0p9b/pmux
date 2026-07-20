package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/0p9b/pmux/internal/app"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

type successEnvelope struct {
	OK   bool `json:"ok"`
	Data any  `json:"data"`
}

type errorEnvelope struct {
	OK    bool      `json:"ok"`
	Error errorView `json:"error"`
}

type errorView struct {
	Code        string        `json:"code"`
	Class       pmuxerr.Class `json:"class,omitempty"`
	Message     string        `json:"message"`
	Explanation string        `json:"explanation,omitempty"`
	Evidence    []string      `json:"evidence,omitempty"`
	Repair      []string      `json:"repair,omitempty"`
	DocsURL     string        `json:"docs_url,omitempty"`
	Cause       string        `json:"cause,omitempty"`
}

func renderResult(w io.Writer, result app.Result, jsonMode bool) error {
	if jsonMode {
		return encodeJSON(w, successEnvelope{OK: true, Data: result.Data})
	}
	if len(result.Human) != 0 {
		_, err := fmt.Fprintln(w, strings.Join(result.Human, "\n"))
		return err
	}
	if result.Data == nil {
		return nil
	}
	if text, ok := result.Data.(string); ok {
		_, err := fmt.Fprintln(w, text)
		return err
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result.Data)
}

func renderEvent(w io.Writer, event app.Event, jsonMode bool) error {
	if jsonMode {
		return encodeJSON(w, event)
	}
	if event.Human != "" {
		_, err := fmt.Fprintln(w, event.Human)
		return err
	}
	if event.Data != nil {
		return renderResult(w, app.Result{Data: event.Data}, false)
	}
	return nil
}

func renderError(w io.Writer, err error, jsonMode, verbose bool) error {
	var typed *pmuxerr.Error
	if !errors.As(err, &typed) {
		typed = pmuxerr.New(pmuxerr.CodeInternal, pmuxerr.Internal, "PMux failed unexpectedly.")
		typed.Cause = err
	}
	if jsonMode {
		view := errorView{
			Code: typed.Code, Class: typed.Class, Message: typed.Message,
			Explanation: typed.Explanation, Evidence: typed.Evidence,
			Repair: typed.Repair, DocsURL: typed.DocsURL,
		}
		if verbose && typed.Cause != nil {
			view.Cause = safeCause(typed.Cause)
		}
		return encodeJSON(w, errorEnvelope{OK: false, Error: view})
	}
	if typed.Code != "" {
		if _, err := fmt.Fprintf(w, "%s: %s\n", typed.Code, typed.Message); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintln(w, typed.Message); err != nil {
		return err
	}
	if typed.Explanation != "" {
		if _, err := fmt.Fprintln(w, "Why:", typed.Explanation); err != nil {
			return err
		}
	}
	if len(typed.Evidence) != 0 {
		if _, err := fmt.Fprintln(w, "Evidence:"); err != nil {
			return err
		}
		for _, evidence := range typed.Evidence {
			if _, err := fmt.Fprintln(w, "  -", evidence); err != nil {
				return err
			}
		}
	}
	if len(typed.Repair) != 0 {
		if _, err := fmt.Fprintln(w, "Do this:"); err != nil {
			return err
		}
		for _, repair := range typed.Repair {
			if _, err := fmt.Fprintln(w, "  -", repair); err != nil {
				return err
			}
		}
	}
	if verbose && typed.Cause != nil {
		_, err := fmt.Fprintln(w, "Cause type:", safeCause(typed.Cause))
		return err
	}
	return nil
}

// safeCause deliberately returns classification only. Calling Error on an
// arbitrary wrapped cause can disclose proxy keys, management secrets, OAuth
// tokens, callback URLs, private keys, or provider-controlled response text.
func safeCause(cause error) string {
	if cause == nil {
		return ""
	}
	return fmt.Sprintf("%T", cause)
}

func encodeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func commandError(err error) error {
	var typed *pmuxerr.Error
	if errors.As(err, &typed) {
		return err
	}
	wrapped := pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, err.Error())
	wrapped.Explanation = "The command line does not match PMux's public grammar."
	wrapped.Repair = []string{"Run `pmux --help` or `pmux <command> --help`."}
	wrapped.Cause = err
	return wrapped
}

func exitCode(err error) int { return pmuxerr.ExitCode(err) }
