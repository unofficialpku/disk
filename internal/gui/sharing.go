package gui

import (
	"net/http"
	"time"

	"github.com/unofficialpku/disk/internal/anyshare"
)

func (a *Application) sharing(w http.ResponseWriter, r *http.Request) {
	c := a.client(w, r, r.URL.Query().Get("account"))
	if c == nil {
		return
	}
	entry, err := c.Client.Resolve(r.Context(), r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, err)
		return
	}
	external := r.URL.Query().Get("external") == "true"
	links, err := c.Client.ShareLinks(r.Context(), entry, external)
	if err != nil {
		writeError(w, err)
		return
	}
	if links == nil {
		links = []anyshare.ShareLink{}
	}
	policy, policyErr := c.Client.SharingPolicy(r.Context(), external)
	result := map[string]any{"links": links, "policy": policy}
	if policyErr != nil {
		result["policyError"] = describeError(policyErr)
	}
	if !external {
		acl, err := c.Client.SharePermissions(r.Context(), entry.ID)
		if err != nil {
			writeError(w, err)
			return
		}
		result["acl"] = acl
	}
	writeJSON(w, result)
}

func (a *Application) recipients(w http.ResponseWriter, r *http.Request) {
	limit, err := queryLimit(r)
	if err != nil {
		writeError(w, err)
		return
	}
	c := a.client(w, r, r.URL.Query().Get("account"))
	if c == nil {
		return
	}
	rows, err := c.Client.SearchRecipients(r.Context(), r.URL.Query().Get("q"), limit)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, rows)
}

func (a *Application) shareAction(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Account      string               `json:"account"`
		Path         string               `json:"path"`
		Action       string               `json:"action"`
		LinkID       string               `json:"linkId"`
		Recipients   []anyshare.Recipient `json:"recipients"`
		Permissions  []string             `json:"permissions"`
		ExpiresHours *float64             `json:"expiresHours"`
		Limit        *int64               `json:"limit"`
		Code         string               `json:"code"`
		Title        string               `json:"title"`
		VerifyMobile bool                 `json:"verifyMobile"`
		Confirm      bool                 `json:"confirm"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Action != "internal_link" && !input.Confirm {
		writeError(w, invalid("confirmation required"))
		return
	}
	opts := anyshare.ShareOptions{Permissions: input.Permissions, Code: input.Code, Title: input.Title, Limit: input.Limit, VerifyMobile: input.VerifyMobile}
	if input.ExpiresHours != nil {
		if *input.ExpiresHours < 0 || *input.ExpiresHours > 24*365*100 {
			writeError(w, invalid("invalid expiration"))
			return
		}
		duration := time.Duration(*input.ExpiresHours * float64(time.Hour))
		opts.ExpiresIn = &duration
	}
	if err := opts.Validate(); err != nil {
		writeError(w, invalid(err.Error()))
		return
	}
	if err := anyshare.ValidateRecipients(input.Recipients); err != nil {
		writeError(w, invalid(err.Error()))
		return
	}
	if (input.Action == "grant" || input.Action == "revoke") && len(input.Recipients) == 0 {
		writeError(w, invalid("select at least one recipient"))
		return
	}
	c := a.client(w, r, input.Account)
	if c == nil {
		return
	}
	if input.Action == "remove_link" {
		if err := c.Client.RemoveExternalLink(r.Context(), input.LinkID); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	entry, err := c.Client.Resolve(r.Context(), input.Path)
	if err != nil {
		writeError(w, err)
		return
	}
	var result any
	switch input.Action {
	case "internal_link":
		result, err = c.Client.InternalLink(r.Context(), entry)
	case "external_link":
		result, err = c.Client.CreateExternalLink(r.Context(), entry, opts)
	case "grant":
		err = c.Client.GrantShare(r.Context(), entry, input.Recipients, opts)
		if err == nil {
			var link anyshare.ShareLink
			link, err = c.Client.InternalLink(r.Context(), entry)
			if err != nil {
				writeJSON(w, map[string]any{"granted": true, "linkError": describeError(err)})
				return
			}
			result = map[string]any{"granted": true, "link": link}
		}
	case "revoke":
		result, err = c.Client.RevokeShare(r.Context(), entry.ID, input.Recipients)
	default:
		err = invalid("unknown sharing operation")
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, result)
}
