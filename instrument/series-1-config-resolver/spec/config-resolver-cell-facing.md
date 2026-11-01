# Task: Layered Configuration Resolver (Go)

> Complete, public task specification handed verbatim to the implementor. Everything required to
> produce a correct implementation is stated here.

## Goal
Implement a Go package `config` that computes an effective configuration by combining four ordered
layers. A lower layer is overridden by a higher one.

## Scope
The package resolves in-memory data. It does not read the environment, any file, or the command
line; the caller supplies all four layer values. The layer names (`defaults`, `file`, `env`,
`flags`) denote precedence only.

## Input and API (fixed)
A layer is a `map[string]any`. A key present in the map is a presented field; its value may be
`false`, `0`, or `""`. A key absent from the map is an absent field. The only legal keys are
`"Verbose"`, `"MaxRetries"`, `"LogLevel"`, `"TimeoutSec"`. A present value of the wrong Go type for
its key is a programmer error and must panic.

```go
func Resolve(defaults, file, env, flags map[string]any) (Config, error)

type Config struct {
    Verbose    bool
    MaxRetries int
    LogLevel   string
    TimeoutSec int
}

var ErrInvalidLogLevel error
```

`Resolve` takes the four maps in precedence order (defaults, file, env, flags) and returns the
effective `Config` and an error.

## Override rule
- Present: the layer sets the field, including when the presented value is a zero value (`false`,
  `0`, `""`). Among the layers that present a field, the highest-precedence one wins.
- Absent: the layer has no effect on the field; the value from lower layers shows through.
- If no layer presents a field: the effective value is that field's Go zero value.

## Validation
Validation applies only to the effective (resolved) `LogLevel`. A presented `LogLevel` that is
overridden by a higher-precedence layer is never validated. The legal effective values are
`"debug"`, `"info"`, `"warn"`, `"error"`, and `""` (an explicit empty level). If the effective
`LogLevel` is anything else, `Resolve` returns an error for which
`errors.Is(err, ErrInvalidLogLevel)` is true. `Verbose`, `MaxRetries`, and `TimeoutSec` are not
validated.

## Deliverables
- The `config` package implementing `Resolve`, `Config`, and `ErrInvalidLogLevel`.
- Builds with `go build`; clean under `go vet`.
- Passes the shipped public test suite (`go test ./...`).
- Idiomatic, maintainable Go. Assume the package lives for years and gains more layers and fields.

## Public test cases (ship with the task; part of acceptance)
1. Only `defaults` presents values → effective equals `defaults`.
2. `file` presents a non-"" `LogLevel` over a non-"" default → the higher layer wins.
3. `flags` presents `Verbose=false` over `defaults` `Verbose=true` → effective `false`.
4. `env` presents `MaxRetries=0` over `defaults` `MaxRetries=3` → effective `0`.
5. `file` presents `LogLevel=""` over `defaults` `LogLevel="info"` → effective `""`, no error.
6. A field absent from `flags` but present in `file` → the `file` value shows through.
7. One field presented in all four layers → the highest-precedence presenting layer wins.
8. Effective `LogLevel` is `"trace"` → `Resolve` returns an error satisfying
   `errors.Is(err, ErrInvalidLogLevel)`.
