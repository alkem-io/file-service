export enum ReadOutputErrorCode {
  USER_NOT_IDENTIFIED = 'user-not-identified',
  NO_AUTH_PROVIDED = 'no-auth-provided',
  DOCUMENT_NOT_FOUND = 'document-not-found',
  FILE_NOT_FOUND = 'file-not-found',
  NO_READ_ACCESS = 'no-read-access',
}
// todo: try to improve the type
export type CanReadOutputData = {
  event: 'can-read-output';
  read: boolean;
  fileName?: string;
  errorCode?: ReadOutputErrorCode;
  error?: string;
};
