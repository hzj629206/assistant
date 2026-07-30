# seatalk-oapi-sdk-go Examples

Run any example from the `seatalk-oapi-sdk-go` directory:

```bash
go run ./examples/basic_client \
  --url "wss://ws-openapi.haiserve.com/ws/bot" \
  --app-id "$APP_ID" \
  --app-secret "$APP_SECRET"
```

Examples:

- `basic_client`: connects, registers, and uses the default dispatcher printers.
- `typed_handlers`: registers typed handlers for the event types supported by the SDK.
- `custom_logging`: replaces the default envelope and invalid-frame printers.

When a registered event handler returns `nil`, the SDK sends `ack` automatically if the event carries a `callback_id`.
