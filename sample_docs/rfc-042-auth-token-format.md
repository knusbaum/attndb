# RFC-042: Authentication Token Format

## Status

Approved. Supersedes RFC-017. Owner: Platform Security team.

## Summary

This RFC defines the format and lifecycle of authentication tokens issued by the
identity service. Tokens are signed JWTs with a fixed set of claims and a bounded
lifetime.

## Token Structure

Each token is a JSON Web Token signed with EdDSA. The payload carries the subject
identifier, the issuing service, the issued-at timestamp, and the expiry. Tokens
must not exceed 4 kilobytes once serialized.

## Token Rotation

Access tokens expire after fifteen minutes. Clients obtain a new access token
using a refresh token, which is rotated on every use. A refresh token that is
replayed after rotation is treated as a compromise and the entire token family is
revoked immediately.

## Retry Policy

When the identity service is unavailable, clients should retry token issuance with
exponential backoff starting at one hundred milliseconds, capped at thirty
seconds, with full jitter. Clients must give up after five minutes and surface an
authentication error to the user.
