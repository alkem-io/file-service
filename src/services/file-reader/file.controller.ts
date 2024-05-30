import {
  Controller,
  Get,
  Param,
  Res,
  StreamableFile,
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
import { FileInfoErrorCode } from './types';
import { FileInfoException } from './exceptions';
import { DocumentData } from './types';

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
    let documentData: DocumentData | undefined;

    try {
      documentData = await this.fileService.readDocument(id, {
        cookie,
        authorization,
      });
    } catch (e) {
      this.logger.error(e, e?.stack);
      throw handleReadErrorByCode(e);
    }

    res.headers({
      'Content-Type': `${documentData.mimeType}`,
      'Cache-Control': 'public, max-age=15552000',
      Pragma: 'public',
      Expires: new Date(Date.now() + 15552000 * 1000).toUTCString(),
    });

    return documentData.file;
  }
}

const handleReadErrorByCode = (err: FileInfoException): HttpException => {
  switch (err.code) {
    case FileInfoErrorCode.USER_NOT_IDENTIFIED:
    case FileInfoErrorCode.NO_READ_ACCESS:
    case FileInfoErrorCode.NO_AUTH_PROVIDED:
      return new ForbiddenException(
        'Insufficient privileges to read this document',
      );
    case FileInfoErrorCode.DOCUMENT_NOT_FOUND:
    case FileInfoErrorCode.FILE_NOT_FOUND:
      return new NotFoundException('Document not found');
    default:
      return new InternalServerErrorException(
        'Unknown error while reading file',
      );
  }
};
