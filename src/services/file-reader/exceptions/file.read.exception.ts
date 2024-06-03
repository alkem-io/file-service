import { BaseException, ExceptionDetails } from '../../../core/exceptions';

export class FileReadException extends BaseException {
  constructor(
    public message: string,
    public details?: ExceptionDetails,
  ) {
    super(message, details);
  }
}
