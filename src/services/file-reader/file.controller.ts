import {
  Controller,
  Get,
  Param,
  Res,
  StreamableFile,
  NotImplementedException,
  Req,
  Headers,
  HttpException,
  InternalServerErrorException,
  ForbiddenException,
  NotFoundException,
  LoggerService,
  Inject,
} from '@nestjs/common';
import { WINSTON_MODULE_NEST_PROVIDER } from 'nest-winston';
import { FastifyReply } from 'fastify';
import { FileService } from './file.service';
import { ReadOutputErrorCode } from './outputs';
import { FileReadException } from './exceptions';

@Controller('/rest/storage')
export class FileController {
  constructor(
    private readonly fileService: FileService,
    @Inject(WINSTON_MODULE_NEST_PROVIDER) private logger: LoggerService,
  ) {}

  @Get('document/:id')
  public async file(
    @Param('id') id: string,
    @Headers('authorization') authorization: string | undefined,
    @Headers('cookie') cookie: string | undefined,
    @Res({ passthrough: true }) res: FastifyReply,
  ): Promise<StreamableFile> {
    return this.fileService
      .readDocument(id, {
        cookie,
        authorization,
      })
      .catch((e) => {
        throw this.handleReadErrorByCode(e);
      });
    // const { read, fileName, error, errorCode } = await this.fileService.canRead(
    //   id,
    //   cookie,
    //   authorization,
    // );
    // return this.fileService.canRead(id, cookie, authorization).catch((e) => {
    //   /* todo catch */
    //   throw new InternalServerErrorException();
    // });
  }

  private handleReadErrorByCode = (err: FileReadException): HttpException => {
    this.logger.error(err);

    switch (err.code) {
      case ReadOutputErrorCode.USER_NOT_IDENTIFIED:
      case ReadOutputErrorCode.NO_READ_ACCESS:
      case ReadOutputErrorCode.NO_AUTH_PROVIDED:
        return new ForbiddenException(
          'Insufficient privileges to read this document',
        );
      case ReadOutputErrorCode.DOCUMENT_NOT_FOUND:
      case ReadOutputErrorCode.FILE_NOT_FOUND:
        return new NotFoundException('Document not found');
      default:
        return new InternalServerErrorException(
          'Unknown error while reading file',
        );
    }
  };
}
