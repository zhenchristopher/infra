-- +goose Up
GRANT trigger_user TO CURRENT_USER;

-- +goose Down
REVOKE trigger_user FROM CURRENT_USER;
