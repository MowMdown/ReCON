package steam

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// WorkshopItem is what Steam says about a Workshop item (a mod).
type WorkshopItem struct {
	ID        uint64
	Title     string
	SizeBytes int64
	Updated   int64 // Unix seconds of its last update
}

// WorkshopItems asks Steam about Workshop items (GetPublishedFileDetails,
// which needs no API key). Items Steam doesn't know are left out.
func WorkshopItems(ctx context.Context, ids []uint64) (map[uint64]WorkshopItem, error) {
	out := map[uint64]WorkshopItem{}
	if len(ids) == 0 {
		return out, nil
	}
	form := url.Values{"itemcount": {strconv.Itoa(len(ids))}}
	for i, id := range ids {
		form.Set(fmt.Sprintf("publishedfileids[%d]", i), strconv.FormatUint(id, 10))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, steamAPIBase+"/ISteamRemoteStorage/GetPublishedFileDetails/v1/", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := steamHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("steam workshop: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("steam workshop: %s", resp.Status)
	}
	var body struct {
		Response struct {
			Details []struct {
				ID      string          `json:"publishedfileid"`
				Result  int             `json:"result"`
				Title   string          `json:"title"`
				Size    json.RawMessage `json:"file_size"`
				Updated int64           `json:"time_updated"`
			} `json:"publishedfiledetails"`
		} `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("steam workshop: %w", err)
	}
	for _, d := range body.Response.Details {
		id, err := strconv.ParseUint(d.ID, 10, 64)
		if err != nil || d.Result != 1 {
			continue
		}
		// file_size is a number or a string depending on the item.
		size, _ := strconv.ParseInt(strings.Trim(string(d.Size), `"`), 10, 64)
		out[id] = WorkshopItem{ID: id, Title: d.Title, SizeBytes: size, Updated: d.Updated}
	}
	return out, nil
}
