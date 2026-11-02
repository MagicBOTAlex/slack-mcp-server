package edge

import (
	"context"
	"encoding/json"
)

// Slack Lists are exposed to browser sessions through the internal
// lists.records.list method. The public slackLists.* API rejects xoxc/xoxd
// tokens with not_allowed_token_type.
type listsRecordsForm struct {
	BaseRequest
	ListID string `json:"list_id"`
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type ListsRecordsResponse struct {
	baseResponse
	Records []json.RawMessage `json:"records"`
	List    json.RawMessage   `json:"list,omitempty"`
}

// ListsRecordsList returns one page of records from a Slack List.
func (cl *Client) ListsRecordsList(ctx context.Context, listID string, limit int, cursor string) (ListsRecordsResponse, error) {
	if listID == "" {
		return ListsRecordsResponse{}, ErrParameterMissing
	}
	form := listsRecordsForm{
		BaseRequest: BaseRequest{Token: cl.token},
		ListID:      listID,
		Limit:       limit,
		Cursor:      cursor,
	}
	resp, err := cl.PostForm(ctx, "lists.records.list", values(form, true))
	if err != nil {
		return ListsRecordsResponse{}, err
	}
	var r ListsRecordsResponse
	if err := cl.ParseResponse(&r, resp); err != nil {
		return ListsRecordsResponse{}, err
	}
	if err := r.validate("lists.records.list"); err != nil {
		return ListsRecordsResponse{}, err
	}
	return r, nil
}
