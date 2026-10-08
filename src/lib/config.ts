/**
 * Validated access to bindings, vars and secrets.
 *
 * Reading config through this module means a missing secret fails loudly at
 * the edge of the request instead of producing `undefined` deep inside a query.
 */
import { ApiError } from './errors';

export interface AppConfig {
  environment: string;
  publicBaseUrl: string;
  defaultShareHours: number;
  maxContentBytes: number;
  agentSecretKey: string;
  sessionSecret: string;
  adminPasswordHash: string;
}

/** Read and validate all configuration. Throws ApiError(500) on misconfiguration. */
export function readConfig(env: Env): AppConfig {
  const publicBaseUrl = requireValue(env.PUBLIC_BASE_URL, 'PUBLIC_BASE_URL').replace(/\/+$/, '');

  return {
    environment: env.ENVIRONMENT ?? 'development',
    publicBaseUrl,
    defaultShareHours: toPositiveInt(env.DEFAULT_SHARE_HOURS, 0),
    maxContentBytes: toPositiveInt(env.MAX_CONTENT_BYTES, 1024 * 1024),
    agentSecretKey: requireValue(env.AGENT_SECRET_KEY, 'AGENT_SECRET_KEY'),
    sessionSecret: requireValue(env.SESSION_SECRET, 'SESSION_SECRET'),
    adminPasswordHash: requireValue(env.ADMIN_PASSWORD_HASH, 'ADMIN_PASSWORD_HASH'),
  };
}

/**
 * Config needed by public, unauthenticated routes.
 *
 * These must not require secrets: an anonymous reader of /api/share/:token
 * should never cause a 500 just because the agent secret is unset.
 */
export function readPublicConfig(env: Env): Pick<AppConfig, 'publicBaseUrl' | 'environment'> {
  return {
    publicBaseUrl: (env.PUBLIC_BASE_URL ?? '').replace(/\/+$/, ''),
    environment: env.ENVIRONMENT ?? 'development',
  };
}

function requireValue(value: string | undefined, name: string): string {
  if (typeof value !== 'string' || value.length === 0) {
    throw ApiError.internal(`Server misconfiguration: ${name} is not set`);
  }
  return value;
}

function toPositiveInt(value: string | undefined, fallback: number): number {
  const parsed = Number.parseInt(value ?? '', 10);
  return Number.isFinite(parsed) && parsed >= 0 ? parsed : fallback;
}
