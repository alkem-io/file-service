import {
  ArgumentsHost,
  Catch,
  ClassProvider,
  ExceptionFilter,
  HttpStatus,
} from '@nestjs/common';
import { APP_FILTER, HttpAdapterHost } from '@nestjs/core';
import { BaseException } from '../exceptions';

type ResponseBody = {
  message: string;
  statusCode: HttpStatus;
  errorId: string;
  path: string;
  timestamp: string;
  stack?: string;
  details?: Record<string, unknown>;
};

@Catch(BaseException)
export class BaseExceptionFilter implements ExceptionFilter {
  constructor(private readonly httpAdapterHost: HttpAdapterHost) {}

  catch(exception: BaseException, host: ArgumentsHost): any {
    // In certain situations `httpAdapter` might not be available in the
    // constructor method, thus we should resolve it here.
    const { httpAdapter } = this.httpAdapterHost;

    const ctx = host.switchToHttp();

    const responseBody: ResponseBody = {
      message: 'An error accured while serving your request',
      statusCode: HttpStatus.INTERNAL_SERVER_ERROR,
      errorId: exception.errorId,
      path: httpAdapter.getRequestUrl(ctx.getRequest()),
      timestamp: new Date().toISOString(),
    };

    if (process.env.NODE_ENV !== 'production') {
      responseBody.stack = exception.stack;
      responseBody.details = exception.details;
    }

    httpAdapter.reply(
      ctx.getResponse(),
      responseBody,
      HttpStatus.INTERNAL_SERVER_ERROR,
    );

    return exception;
  }
}

export const BaseExceptionFilterProvider: ClassProvider<BaseExceptionFilter> = {
  provide: APP_FILTER,
  useClass: BaseExceptionFilter,
};
