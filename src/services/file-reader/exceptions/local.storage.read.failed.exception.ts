import { BaseException, ExceptionDetails } from '../../../core/exceptions';

export class LocalStorageReadFailedException extends BaseException {
  constructor(message: string, details?: ExceptionDetails) {
    super(message, details);
  }
}
