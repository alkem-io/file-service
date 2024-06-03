import { BaseException, ExceptionDetails } from '../../../core/exceptions';
import { FileInfoErrorCode } from '../types';

export class FileInfoException extends BaseException {
  constructor(
    public message: string,
    public code: FileInfoErrorCode,
    public details?: ExceptionDetails,
  ) {
    super(message, details);
  }
}
