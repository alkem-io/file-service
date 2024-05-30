import { FileInfoErrorCode } from '../types';

type Data = {
  read: boolean;
};
type WithErrorData = Data & {
  errorCode: FileInfoErrorCode;
  error: string;
};
type WithSuccessData = Data & {
  fileName: string;
  mimeType: string;
};
// todo: not the best type
export type FileInfoOutputData = { data: WithErrorData | WithSuccessData };

export const isFileInfoOutputWithErrorData = (
  data: WithErrorData | WithSuccessData,
): data is WithErrorData => {
  return !!(data as WithErrorData)?.errorCode;
};
