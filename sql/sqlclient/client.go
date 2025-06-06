// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package sqlclient

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sync"

	"github.com/veiloq/atlas/schemahcl"
	"github.com/veiloq/atlas/sql/migrate"
	"github.com/veiloq/atlas/sql/schema"
)

type (
	// Client provides the common functionalities for working with Atlas from different
	// applications (e.g. CLI and TF). Note, the Client is dialect specific and should
	// be instantiated using a call to Open.
	Client struct {
		// Name used when creating the client.
		Name string

		// DB used for creating the client.
		DB *sql.DB
		// URL holds an enriched url.URL.
		URL *URL

		// A migration driver for the attached dialect.
		migrate.Driver
		// Additional closers that can be closed at the
		// end of the client lifetime.
		closers []io.Closer

		// Marshal and Evaluator functions for decoding
		// and encoding the schema documents.
		schemahcl.Marshaler
		schemahcl.Evaluator

		// Ephemeral indicates that the database we connect to is "ephemeral"
		// (e.g., a temporary running container). This can be set by the driver
		// that opens the client to signal to its consumers that there is no need
		// to guard against race conditions with other Atlas clients.
		Ephemeral bool

		// Functions registered by the drivers and used for opening transactions and their clients.
		openDriver func(schema.ExecQuerier) (migrate.Driver, error)
		openTx     TxOpener
		hooks      []*Hook
	}

	// TxClient is returned by calling Client.Tx. It behaves the same as Client,
	// but wraps all operations within a transaction.
	TxClient struct {
		*Client

		// The transaction this Client wraps.
		Tx    *Tx
		hooks []*Hook
	}

	// URL extends the standard url.URL with additional
	// connection information attached by the Opener (if any).
	URL struct {
		*url.URL

		// The DSN used for opening the connection.
		DSN string `json:"-"`

		// The Schema this client is connected to.
		Schema string
	}

	// Hook groups all possible hooks in
	// connection and transaction lifecycle.
	Hook struct {
		Conn struct {
			AfterOpen   func(context.Context, *Client) error
			BeforeClose func(*Client) error
		}
		Tx struct {
			AfterBegin func(context.Context, *TxClient) error
			BeforeCommit,
			BeforeRollback func(*TxClient) error
		}
	}
)

// Tx returns a transactional client.
func (c *Client) Tx(ctx context.Context, opts *sql.TxOptions) (*TxClient, error) {
	if c.openDriver == nil {
		return nil, errors.New("sql/sqlclient: unexpected driver opener: <nil>")
	}
	var tx *Tx
	switch {
	case c.openTx != nil:
		ttx, err := c.openTx(ctx, c.DB, opts)
		if err != nil {
			return nil, err
		}
		tx = ttx
	default:
		ttx, err := c.DB.BeginTx(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("sql/sqlclient: starting transaction: %w", err)
		}
		tx = &Tx{Tx: ttx}
	}
	drv, err := c.openDriver(tx)
	if err != nil {
		return nil, fmt.Errorf("sql/sqlclient: opening atlas driver: %w", err)
	}
	ic := *c
	ic.Driver = drv
	tc := &TxClient{Client: &ic, Tx: tx, hooks: c.hooks}
	if len(tc.hooks) > 0 {
		if err := tc.afterBegin(ctx); err != nil {
			return nil, errors.Join(err, tx.Rollback())
		}
	}
	return tc, nil
}

// Commit the transaction.
func (c *TxClient) Commit() error {
	return errors.Join(c.beforeCommit(), c.Tx.Commit())
}

// Rollback the transaction.
func (c *TxClient) Rollback() error {
	return errors.Join(c.beforeRollback(), c.Tx.Rollback())
}

// AddClosers adds list of closers to close at the end of the client lifetime.
func (c *Client) AddClosers(closers ...io.Closer) {
	c.closers = append(c.closers, closers...)
}

// Close closes the underlying database connection and the migration
// driver in case it implements the io.Closer interface.
func (c *Client) Close() error {
	err := c.beforeClose()
	for _, closer := range append(c.closers, c.DB) {
		err = errors.Join(err, closer.Close())
	}
	return err
}

type hookCtxKey struct{}

// hookCtx marks the context as being in a hook.
func hookCtx(ctx context.Context) context.Context {
	if ctx.Value(hookCtxKey{}) == nil {
		return context.WithValue(ctx, hookCtxKey{}, true)
	}
	return ctx
}

// afterOpen calls the AfterOpen hooks.
func (c *Client) afterOpen(ctx context.Context) error {
	if ctx.Value(hookCtxKey{}) != nil {
		return errors.New("sql/sqlclient: cannot open a connection inside a hook")
	}
	for _, h := range c.hooks {
		if f := h.Conn.AfterOpen; f != nil {
			if err := f(hookCtx(ctx), c); err != nil {
				return err
			}
		}
	}
	return nil
}

// beforeClose calls the BeforeClose hooks.
func (c *Client) beforeClose() error {
	for _, h := range c.hooks {
		if f := h.Conn.BeforeClose; f != nil {
			if err := f(c); err != nil {
				return err
			}
		}
	}
	return nil
}

// afterBegin calls the AfterBegin hooks.
func (c *TxClient) afterBegin(ctx context.Context) error {
	if ctx.Value(hookCtxKey{}) != nil {
		return errors.New("sql/sqlclient: cannot begin a transaction inside a hook")
	}
	for _, h := range c.hooks {
		if f := h.Tx.AfterBegin; f != nil {
			if err := f(hookCtx(ctx), c); err != nil {
				return err
			}
		}
	}
	return nil
}

// beforeCommit calls the BeforeCommit hooks.
func (c *TxClient) beforeCommit() error {
	for _, h := range c.hooks {
		if f := h.Tx.BeforeCommit; f != nil {
			if err := f(c); err != nil {
				return err
			}
		}
	}
	return nil
}

// beforeRollback calls the BeforeRollback hooks.
func (c *TxClient) beforeRollback() error {
	for _, h := range c.hooks {
		if f := h.Tx.BeforeRollback; f != nil {
			if err := f(c); err != nil {
				return err
			}
		}
	}
	return nil
}

type (
	// Opener opens a migration driver by the given URL.
	Opener interface {
		Open(ctx context.Context, u *url.URL) (*Client, error)
	}

	// OpenerFunc allows using a function as an Opener.
	OpenerFunc func(context.Context, *url.URL) (*Client, error)

	// URLParser parses an url.URL into an enriched URL and attaches additional info to it.
	URLParser interface {
		ParseURL(*url.URL) *URL
	}

	// URLParserFunc allows using a function as an URLParser.
	URLParserFunc func(*url.URL) *URL

	// SchemaChanger is implemented by a driver if it how to change the connection URL to represent another schema.
	SchemaChanger interface {
		ChangeSchema(*url.URL, string) *url.URL
	}

	driver struct {
		Opener
		name     string
		parser   URLParser
		txOpener TxOpener
	}
)

// Open calls f(ctx, u).
func (f OpenerFunc) Open(ctx context.Context, u *url.URL) (*Client, error) {
	return f(ctx, u)
}

// ParseURL calls f(u).
func (f URLParserFunc) ParseURL(u *url.URL) *URL {
	return f(u)
}

// ParseURL is similar to url.Parse but returns errors without
// the raw URL attached to avoid printing userinfo in errors.
func ParseURL(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		if err1 := (*url.Error)(nil); errors.As(err, &err1) {
			err = err1.Err
		}
		return nil, err
	}
	return u, nil
}

var drivers sync.Map

type (
	// openOptions holds additional configuration values for opening a Client.
	openOptions struct {
		schema *string
		hooks  []*Hook
	}
	// OpenOption allows to configure a openOptions using functional arguments.
	OpenOption func(*openOptions) error
)

// ErrUnsupported is returned if a registered driver does not support changing the schema.
var ErrUnsupported = errors.New("sql/sqlclient: driver does not support changing connected schema")

// Open opens an Atlas client by its provided url string.
func Open(ctx context.Context, s string, opts ...OpenOption) (*Client, error) {
	u, err := ParseURL(s)
	if err != nil {
		return nil, fmt.Errorf("sql/sqlclient: parse open url: %w", err)
	}
	return OpenURL(ctx, u, opts...)
}

// HasDriver reports if there is any driver registered with the given scheme.
func HasDriver(scheme string) bool {
	_, ok := drivers.Load(scheme)
	return ok
}

// OpenURL opens an Atlas client by its provided url.URL.
func OpenURL(ctx context.Context, u *url.URL, opts ...OpenOption) (*Client, error) {
	cfg := &openOptions{}
	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			return nil, err
		}
	}
	if u.Scheme == "" {
		return nil, errors.New("sql/sqlclient: missing driver. See: https://atlasgo.io/url")
	}
	v, ok := drivers.Load(u.Scheme)
	if !ok {
		return nil, fmt.Errorf("sql/sqlclient: unknown driver %q. See: https://atlasgo.io/url", u.Scheme)
	}
	drv := v.(*driver)
	// If there is a schema given and the driver allows to change the schema for the url, do it.
	if cfg.schema != nil {
		sc, ok := drv.parser.(SchemaChanger)
		if !ok {
			return nil, ErrUnsupported
		}
		u = sc.ChangeSchema(u, *cfg.schema)
	}
	client, err := drv.Open(ctx, u)
	if err != nil {
		return nil, err
	}
	if client.URL == nil {
		client.URL = drv.parser.ParseURL(u)
	}
	if client.openTx == nil && drv.txOpener != nil {
		client.openTx = drv.txOpener
	}
	if len(cfg.hooks) > 0 {
		client.hooks = cfg.hooks
		if err := client.afterOpen(ctx); err != nil {
			return nil, errors.Join(err, client.DB.Close())
		}
	}
	return client, nil
}

// OpenSchema opens the connection to the given schema.
// If the registered driver does not support this, ErrUnsupported is returned instead.
func OpenSchema(s string) OpenOption {
	return func(c *openOptions) error {
		c.schema = &s
		return nil
	}
}

// OpenWithHooks returns an OpenOption that sets
// the hooks for the client after opening.
func OpenWithHooks(hks ...*Hook) OpenOption {
	return func(c *openOptions) error {
		c.hooks = append(c.hooks, hks...)
		return nil
	}
}

type (
	registerOptions struct {
		openDriver func(schema.ExecQuerier) (migrate.Driver, error)
		txOpener   TxOpener
		parser     URLParser
		flavours   []string
		schemahcl.Marshaler
		schemahcl.Evaluator
	}
	// RegisterOption allows configuring the Opener
	// registration using functional options.
	RegisterOption func(*registerOptions)
)

// RegisterFlavours allows registering additional flavours
// (i.e. names), accepted by Atlas to open clients.
func RegisterFlavours(flavours ...string) RegisterOption {
	return func(opts *registerOptions) {
		opts.flavours = flavours
	}
}

// RegisterURLParser allows registering a function for parsing
// the url.URL and attach additional info to the extended URL.
func RegisterURLParser(p URLParser) RegisterOption {
	return func(opts *registerOptions) {
		opts.parser = p
	}
}

// RegisterCodec registers static codec for attaching into
// the client after it is opened.
func RegisterCodec(m schemahcl.Marshaler, e schemahcl.Evaluator) RegisterOption {
	return func(opts *registerOptions) {
		opts.Marshaler, opts.Evaluator = m, e
	}
}

// RegisterDriverOpener registers a func to create a migrate.Driver from a schema.ExecQuerier.
// Registering this function is implicitly done when using DriverOpener.
// The passed opener is used when creating a TxClient.
func RegisterDriverOpener(open func(schema.ExecQuerier) (migrate.Driver, error)) RegisterOption {
	return func(opts *registerOptions) {
		opts.openDriver = open
	}
}

// DriverOpener is a helper Opener creator for sharing between all drivers.
func DriverOpener(open func(schema.ExecQuerier) (migrate.Driver, error)) Opener {
	return OpenerFunc(func(_ context.Context, u *url.URL) (*Client, error) {
		v, ok := drivers.Load(u.Scheme)
		if !ok {
			return nil, fmt.Errorf("sql/sqlclient: unexpected missing opener %q", u.Scheme)
		}
		drv := v.(*driver)
		ur := drv.parser.ParseURL(u)
		db, err := sql.Open(drv.name, ur.DSN)
		if err != nil {
			return nil, err
		}
		mdr, err := open(db)
		if err != nil {
			if cerr := db.Close(); cerr != nil {
				err = fmt.Errorf("%w: %v", err, cerr)
			}
			return nil, err
		}
		return &Client{
			Name:       drv.name,
			DB:         db,
			URL:        ur,
			Driver:     mdr,
			openDriver: open,
			openTx:     drv.txOpener,
		}, nil
	})
}

type (
	// Tx wraps sql.Tx with optional custom Commit and Rollback functions.
	Tx struct {
		*sql.Tx
		CommitFn   func() error // override default commit behavior
		RollbackFn func() error // override default rollback behavior
	}
	// TxOpener opens a transaction with optional closer.
	TxOpener func(context.Context, *sql.DB, *sql.TxOptions) (*Tx, error)
)

// Commit the transaction.
func (tx *Tx) Commit() error {
	fn := tx.CommitFn
	if fn == nil {
		fn = tx.Tx.Commit
	}
	return fn()
}

// Rollback the transaction.
func (tx *Tx) Rollback() error {
	fn := tx.RollbackFn
	if fn == nil {
		fn = tx.Tx.Rollback
	}
	return fn()
}

// RegisterTxOpener allows registering a custom transaction opener with an optional close function.
func RegisterTxOpener(open TxOpener) RegisterOption {
	return func(opts *registerOptions) {
		opts.txOpener = open
	}
}

// Register registers a client Opener (i.e. creator) with the given name.
func Register(name string, opener Opener, opts ...RegisterOption) {
	if opener == nil {
		panic("sql/sqlclient: Register opener is nil")
	}
	opt := &registerOptions{
		// Default URL parser uses the URL as the DSN.
		parser: URLParserFunc(func(u *url.URL) *URL { return &URL{URL: u, DSN: u.String()} }),
	}
	for i := range opts {
		opts[i](opt)
	}
	if opt.Marshaler != nil && opt.Evaluator != nil {
		f := opener
		opener = OpenerFunc(func(ctx context.Context, u *url.URL) (*Client, error) {
			c, err := f.Open(ctx, u)
			if err != nil {
				return nil, err
			}
			c.Marshaler, c.Evaluator = opt.Marshaler, opt.Evaluator
			return c, nil
		})
	}
	// If there was a driver opener registered by a call to RegisterDriverOpener, it has precedence.
	if opt.openDriver != nil {
		f := opener
		opener = OpenerFunc(func(ctx context.Context, u *url.URL) (*Client, error) {
			c, err := f.Open(ctx, u)
			if err != nil {
				return nil, err
			}
			c.openDriver = opt.openDriver
			return c, err
		})
	}
	drv := &driver{Opener: opener, name: name, parser: opt.parser, txOpener: opt.txOpener}
	for _, f := range append(opt.flavours, name) {
		if _, ok := drivers.Load(f); ok {
			panic("sql/sqlclient: Register called twice for " + f)
		}
		drivers.Store(f, drv)
	}
}
