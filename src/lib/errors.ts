/**
 * Structured HTTP errors.
 *
 * Every handler throws `ApiError`; the global `onError` in src/index.ts turns
 * it into a JSON envelope. This keeps error shapes consistent and prevents
 * internal messages from leaking to clients.
 */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  /** Extra fields merged into the response body (e.g. `{ field: 'title' }`). */
  readonly details?: Record<string, unknown>;

  constructor(
    status: number,
    code: string,
    message: string,
    details?: Record<string, unknown>,
  ) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.details = details;
  }

  static badRequest(message: string, details?: Record<string, unknown>) {
    return new ApiError(400, 'bad_request', message, details);
  }

  static unauthorized(message = 'Missing or invalid credentials') {
    return new ApiError(401, 'unauthorized', message);
  }

  static forbidden(message = 'Not allowed') {
    return new ApiError(403, 'forbidden', message);
  }

  /**
   * Used for unknown, revoked and expired tokens alike, so an attacker cannot
   * distinguish "never existed" from "was revoked".
   */
  static notFound(message = 'Report or share link not found') {
    return new ApiError(404, 'not_found', message);
  }

  static gone(message = 'This share link has expired') {
    return new ApiError(410, 'gone', message);
  }

  static payloadTooLarge(message: string) {
    return new ApiError(413, 'payload_too_large', message);
  }

  static internal(message = 'Internal server error') {
    return new ApiError(500, 'internal_error', message);
  }
}

/** Canonical success/error envelope returned by the API. */
export interface ApiEnvelope<T> {
  ok: boolean;
  data?: T;
  error?: { code: string; message: string; details?: Record<string, unknown> };
  requestId?: string;
}

export function ok<T>(data: T, requestId?: string): ApiEnvelope<T> {
  return { ok: true, data, requestId };
}

export function fail(
  code: string,
  message: string,
  details?: Record<string, unknown>,
  requestId?: string,
): ApiEnvelope<never> {
  return { ok: false, error: { code, message, details }, requestId };
}
