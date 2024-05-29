import {
  BaseException,
  ExceptionDetails,
} from '../../../core/exceptions/base.exception';
import { ReadOutputErrorCode } from '../outputs';

export class FileReadException extends BaseException {
  constructor(
    public message: string,
    public code: ReadOutputErrorCode,
    public details?: ExceptionDetails,
  ) {
    super(message, details);
  }
}
