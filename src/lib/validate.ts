/**
 * Request payload validation.
 *
 * Hand-rolled and dependency-free on purpose: the accepted surface is small and
 * this keeps the Worker bundle tiny. Every function throws `ApiError(400)` with
 * a `field` detail so agents get actionable feedback.
 */
import { ApiError } from './errors';
import type { CreateReportInput, ReportFormat } from '../types';

const FORMATS: ReadonlySet<string> = new Set(['markdown', 'html']);
const CATEGORY_RE = /^[a-z0-9][a-z0-9_-]{0,31}$/;
const TITLE_MAX = 300;

export function parseCreateReportInput(
  body: unknown,
  limits: { maxContentBytes: number },
): CreateReportInput {
  if (!isPlainObject(body)) {
    throw ApiError.badRequest('Request body must be a JSON object');
  }

  const title = requireString(body.title, 'title', 1, TITLE_MAX);

  const content = body.content;
  if (typeof content !== 'string' || content.length === 0) {
    throw ApiError.badRequest('`content` is required and must be a non-empty string', { field: 'content' });
  }
  // Measure bytes, not UTF-16 code units: D1 row limits are byte-based and CJK
  // content is ~3 bytes per character.
  const contentBytes = new TextEncoder().encode(content).byteLength;
  if (contentBytes > limits.maxContentBytes) {
    throw ApiError.payloadTooLarge(
      `\`content\` is ${contentBytes} bytes, exceeding the ${limits.maxContentBytes} byte limit`,
    );
  }

  let category = 'general';
  if (body.category !== undefined && body.category !== null && body.category !== '') {
    if (typeof body.category !== 'string' || !CATEGORY_RE.test(body.category)) {
      throw ApiError.badRequest(
        '`category` must match /^[a-z0-9][a-z0-9_-]{0,31}$/ (e.g. pentest, architecture, progress)',
        { field: 'category' },
      );
    }
    category = body.category;
  }

  let format: ReportFormat = 'markdown';
  if (body.format !== undefined && body.format !== null && body.format !== '') {
    if (typeof body.format !== 'string' || !FORMATS.has(body.format)) {
      throw ApiError.badRequest('`format` must be one of: markdown, html', { field: 'format' });
    }
    format = body.format as ReportFormat;
  }

  let metadata: Record<string, unknown> | undefined;
  if (body.metadata !== undefined && body.metadata !== null) {
    if (!isPlainObject(body.metadata)) {
      throw ApiError.badRequest('`metadata` must be a JSON object', { field: 'metadata' });
    }
    metadata = body.metadata;
  }

  let autoShareHours: number | undefined;
  if (body.autoShareHours !== undefined && body.autoShareHours !== null) {
    const n = Number(body.autoShareHours);
    if (!Number.isFinite(n) || n < 0 || n > 24 * 365 * 10) {
      throw ApiError.badRequest(
        '`autoShareHours` must be a number between 0 and 87600 (0 = never expires)',
        { field: 'autoShareHours' },
      );
    }
    autoShareHours = n;
  }

  return { title, category, format, content, metadata, autoShareHours };
}

/**
 * Read a required route parameter.
 *
 * Hono types path params as `string | undefined`; this narrows it once at the
 * boundary instead of scattering non-null assertions through the routes.
 */
export function requiredParam(value: string | undefined, name = 'param'): string {
  if (typeof value !== 'string' || value.length === 0) {
    throw ApiError.badRequest(`Missing route parameter: ${name}`);
  }
  return value;
}

/** Validate a share token shape before it ever reaches SQL. */
export function parseShareToken(raw: string): string {
  if (typeof raw !== 'string' || !/^[A-Za-z0-9_-]{16,64}$/.test(raw)) {
    throw ApiError.notFound();
  }
  return raw;
}

export function parseJsonBody(raw: string): unknown {
  try {
    return JSON.parse(raw);
  } catch {
    throw ApiError.badRequest('Request body is not valid JSON');
  }
}

export function requireString(
  value: unknown,
  field: string,
  min = 1,
  max = 255,
): string {
  if (typeof value !== 'string') {
    throw ApiError.badRequest(`\`${field}\` is required and must be a string`, { field });
  }
  const trimmed = value.trim();
  if (trimmed.length < min || trimmed.length > max) {
    throw ApiError.badRequest(
      `\`${field}\` must be between ${min} and ${max} characters`,
      { field },
    );
  }
  return trimmed;
}

function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
